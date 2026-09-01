package davmigration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tenbyte/mail-migrator/internal/database"
	"github.com/tenbyte/mail-migrator/internal/dav"
	"github.com/tenbyte/mail-migrator/internal/domain"
	"github.com/tenbyte/mail-migrator/internal/retry"
	"github.com/tenbyte/mail-migrator/internal/security"
)

type EventSink func(domain.Progress)

type Service struct {
	db      *database.DB
	factory dav.Factory
	events  EventSink
	mu      sync.Mutex
	runs    map[string]*runControl
}

type runControl struct {
	cancel context.CancelFunc
	paused atomic.Bool
	notify chan struct{}
}

func New(db *database.DB, factory dav.Factory, events EventSink) *Service {
	return &Service{db: db, factory: factory, events: events, runs: make(map[string]*runControl)}
}

func (s *Service) Active() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.runs) > 0
}

func (s *Service) Inspect(ctx context.Context, kind domain.ServiceKind, endpoint domain.DAVEndpoint) (domain.DAVAccountSummary, error) {
	client, err := s.factory.Connect(ctx, kind, endpoint, 15*time.Second, 90*time.Second)
	if err != nil {
		return domain.DAVAccountSummary{}, err
	}
	return client.Summary(ctx)
}

func (s *Service) Preflight(ctx context.Context, kind domain.ServiceKind, source, destination domain.DAVEndpoint) (domain.DAVPreflightResult, error) {
	type result struct {
		source  bool
		client  dav.Client
		summary domain.DAVAccountSummary
		err     error
	}
	results := make(chan result, 2)
	connect := func(endpoint domain.DAVEndpoint, sourceSide bool) {
		client, err := s.factory.Connect(ctx, kind, endpoint, 15*time.Second, 90*time.Second)
		if err != nil {
			results <- result{source: sourceSide, err: err}
			return
		}
		summary, err := client.Summary(ctx)
		results <- result{source: sourceSide, client: client, summary: summary, err: err}
	}
	go connect(source, true)
	go connect(destination, false)
	first, second := <-results, <-results
	if first.err != nil {
		return domain.DAVPreflightResult{}, first.err
	}
	if second.err != nil {
		return domain.DAVPreflightResult{}, second.err
	}
	var sourceResult, destinationResult result
	if first.source {
		sourceResult, destinationResult = first, second
	} else {
		sourceResult, destinationResult = second, first
	}
	mappings := recommendMappings(sourceResult.summary.Collections, destinationResult.summary.Collections)
	warnings := append([]string{}, sourceResult.summary.Warnings...)
	warnings = append(warnings, destinationResult.summary.Warnings...)
	probed := make(map[string]bool)
	for _, mapping := range mappings {
		if !mapping.DestinationExists || mapping.DestinationPath == "" || probed[cleanPath(mapping.DestinationPath)] {
			continue
		}
		if err := destinationResult.client.Probe(ctx, mapping.DestinationPath); err != nil {
			return domain.DAVPreflightResult{}, fmt.Errorf("write test for %q: %w", mapping.DestinationName, err)
		}
		probed[cleanPath(mapping.DestinationPath)] = true
	}
	if len(probed) == 0 {
		warnings = append(warnings, "The destination does not contain a matching collection yet; the write test runs after it is created.")
	}
	for _, mapping := range mappings {
		if mapping.Source.MaxResourceSize > 0 && mapping.DestinationExists {
			for _, target := range destinationResult.summary.Collections {
				if target.Path == mapping.DestinationPath && target.MaxResourceSize > 0 && mapping.Source.MaxResourceSize > target.MaxResourceSize {
					warnings = append(warnings, fmt.Sprintf("%s: The destination reports a lower resource limit than the source.", mapping.Source.Name))
				}
			}
		}
	}
	inspection := domain.DAVPreflightResult{Kind: kind, Source: sourceResult.summary, Destination: destinationResult.summary, Mappings: mappings, Problems: []domain.ConversionWarning{}, Warnings: warnings}
	if err := inspectPreflightObjects(ctx, sourceResult.client, destinationResult.client, &inspection); err != nil {
		return inspection, err
	}
	if len(inspection.Problems) > 0 {
		inspection.Warnings = append(inspection.Warnings, fmt.Sprintf("%d object(s) cannot be converted safely and will be skipped individually.", len(inspection.Problems)))
	}
	for _, collection := range destinationResult.summary.Collections {
		if collection.QuotaAvailableBytes > 0 && sourceResult.summary.Bytes > collection.QuotaAvailableBytes {
			inspection.Warnings = append(inspection.Warnings, "The destination reports less free storage than the source data requires.")
			break
		}
	}
	return inspection, nil
}

func inspectPreflightObjects(ctx context.Context, source, destination dav.Client, result *domain.DAVPreflightResult) error {
	destinationByName := collectionMapByName(result.Destination.Collections)
	for _, mapping := range result.Mappings {
		if !mapping.Enabled {
			continue
		}
		target := findDestinationCollection(database.DAVCollectionRecord{DestinationPath: mapping.DestinationPath, DestinationName: mapping.DestinationName}, result.Destination.Collections, destinationByName)
		targetResources := map[string]bool{}
		if target.Path != "" {
			inventory, err := destination.Inventory(ctx, target.Path, "", 500)
			if err != nil {
				return fmt.Errorf("read destination collection %q for dry run: %w", mapping.DestinationName, err)
			}
			for _, resource := range inventory.Resources {
				targetResources[cleanPath(resource.Href)] = true
			}
		}
		inventory, err := source.Inventory(ctx, mapping.Source.Path, "", 500)
		if err != nil {
			return fmt.Errorf("read source collection %q for dry run: %w", mapping.Source.Name, err)
		}
		for _, resource := range inventory.Resources {
			result.ObjectsScanned++
			if target.Path != "" && targetResources[cleanPath(destinationHref(target.Path, resource.Href, result.Kind))] {
				result.PotentialConflicts++
			}
			fetched, err := source.Get(ctx, resource.Href, mapping.Source.MaxResourceSize)
			if err != nil {
				result.Problems = append(result.Problems, domain.ConversionWarning{ResourceHref: resource.Href, Kind: result.Kind, Code: "TB-DAV-PREFLIGHT-READ-001", Message: "The object could not be read during the dry run."})
				continue
			}
			data, readErr := io.ReadAll(fetched.Body)
			closeErr := fetched.Body.Close()
			if readErr != nil || closeErr != nil {
				result.Problems = append(result.Problems, domain.ConversionWarning{ResourceHref: resource.Href, Kind: result.Kind, Code: "TB-DAV-PREFLIGHT-READ-001", Message: "The object could not be read completely during the dry run."})
				continue
			}
			transformed, transformErr := dav.Transform(result.Kind, data, target)
			if transformErr != nil {
				result.Problems = append(result.Problems, domain.ConversionWarning{ResourceHref: resource.Href, Kind: result.Kind, Code: "TB-DAV-PREFLIGHT-CONVERT-001", Message: transformErr.Error()})
				continue
			}
			if len(transformed.Warnings) > 0 {
				result.Conversions++
			}
		}
	}
	return nil
}

func recommendMappings(source, destination []domain.DAVCollection) []domain.CollectionMapping {
	byName := make(map[string]domain.DAVCollection, len(destination))
	for _, collection := range destination {
		byName[strings.ToLower(strings.TrimSpace(collection.Name))] = collection
	}
	result := make([]domain.CollectionMapping, 0, len(source))
	for _, collection := range source {
		mapping := domain.CollectionMapping{Source: collection, DestinationName: collection.Name, Enabled: true}
		if target, ok := byName[strings.ToLower(strings.TrimSpace(collection.Name))]; ok {
			mapping.DestinationPath = target.Path
			mapping.DestinationName = target.Name
			mapping.DestinationExists = true
		}
		result = append(result, mapping)
	}
	return result
}

func (s *Service) Start(parent context.Context, migrationID int64, request domain.DAVServiceRequest, options domain.TransferOptions, mode string) error {
	if migrationID <= 0 || !request.Enabled {
		return errors.New("invalid DAV migration request")
	}
	if options.Concurrency <= 0 {
		options = domain.DefaultTransferOptions()
	}
	key := runKey(migrationID, request.Kind)
	ctx, cancel := context.WithCancel(context.Background())
	control := &runControl{cancel: cancel, notify: make(chan struct{}, 1)}
	s.mu.Lock()
	if _, exists := s.runs[key]; exists {
		s.mu.Unlock()
		cancel()
		return errors.New("DAV migration is already running")
	}
	s.runs[key] = control
	s.mu.Unlock()
	go func() {
		defer func() {
			s.mu.Lock()
			delete(s.runs, key)
			s.mu.Unlock()
		}()
		s.run(ctx, migrationID, request, options, mode, control)
	}()
	return nil
}

func (s *Service) Pause(migrationID int64) error {
	controls := s.controls(migrationID)
	if len(controls) == 0 {
		return errors.New("DAV migration is not running")
	}
	for kind, control := range controls {
		control.paused.Store(true)
		_ = s.db.MarkService(context.Background(), migrationID, kind, domain.MigrationPaused, "")
	}
	return nil
}

func (s *Service) Resume(migrationID int64) error {
	controls := s.controls(migrationID)
	if len(controls) == 0 {
		return errors.New("DAV migration is not running")
	}
	for kind, control := range controls {
		control.paused.Store(false)
		select {
		case control.notify <- struct{}{}:
		default:
		}
		_ = s.db.MarkService(context.Background(), migrationID, kind, domain.MigrationRunning, "")
	}
	return nil
}

func (s *Service) Cancel(migrationID int64) error {
	controls := s.controls(migrationID)
	if len(controls) == 0 {
		return errors.New("DAV migration is not running")
	}
	for _, control := range controls {
		control.cancel()
	}
	return nil
}

func (s *Service) controls(migrationID int64) map[domain.ServiceKind]*runControl {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := map[domain.ServiceKind]*runControl{}
	for _, kind := range []domain.ServiceKind{domain.ServiceCalendar, domain.ServiceContacts} {
		if control := s.runs[runKey(migrationID, kind)]; control != nil {
			result[kind] = control
		}
	}
	return result
}

func runKey(id int64, kind domain.ServiceKind) string { return fmt.Sprintf("%d:%s", id, kind) }

func (s *Service) run(ctx context.Context, migrationID int64, request domain.DAVServiceRequest, options domain.TransferOptions, mode string, control *runControl) {
	kind := request.Kind
	_ = s.db.MarkService(context.Background(), migrationID, kind, domain.MigrationRunning, "")
	started := time.Now()
	progresses, _ := s.db.ServiceProgresses(ctx, migrationID)
	var total, done, failed, totalBytes, doneBytes atomic.Int64
	var runTotal, runDone atomic.Int64
	var runPhase atomic.Value
	var runIndeterminate atomic.Bool
	runPhase.Store("Inventory")
	runIndeterminate.Store(true)
	runTotal.Store(1)
	for _, progress := range progresses {
		if progress.Kind == kind {
			total.Store(progress.ItemsTotal)
			done.Store(progress.ItemsDone)
			failed.Store(progress.ItemsFailed)
			totalBytes.Store(progress.BytesTotal)
			doneBytes.Store(progress.BytesDone)
		}
	}
	emit := func(current, lastError string, state domain.MigrationState) {
		if s.events == nil {
			return
		}
		elapsed := time.Since(started).Seconds()
		var speed float64
		if elapsed > 0 {
			speed = float64(doneBytes.Load()) / elapsed
		}
		s.events(domain.Progress{MigrationID: migrationID, Service: kind, State: state, CurrentFolder: current, MessagesTotal: total.Load(), MessagesCopied: done.Load(), MessagesFailed: failed.Load(), BytesTotal: totalBytes.Load(), BytesCopied: doneBytes.Load(), BytesPerSecond: speed, StartedAt: started, LastError: lastError, RunMode: mode, RunPhase: runPhase.Load().(string), RunItemsTotal: runTotal.Load(), RunItemsDone: runDone.Load(), RunIndeterminate: runIndeterminate.Load()})
	}
	emit("Preparing operation", "", domain.MigrationRunning)
	failRun := func(err error) {
		message := security.RedactError(err.Error(), request.Source.Password, request.Destination.Password)
		_ = s.db.MarkService(context.Background(), migrationID, kind, domain.MigrationFailed, message)
		emit("", message, domain.MigrationFailed)
	}
	timeout := time.Duration(options.ConnectionTimeout) * time.Second
	stall := time.Duration(options.StallTimeout) * time.Second
	source, err := s.factory.Connect(ctx, kind, request.Source, timeout, stall)
	if err != nil {
		failRun(err)
		return
	}
	destination, err := s.factory.Connect(ctx, kind, request.Destination, timeout, stall)
	if err != nil {
		failRun(err)
		return
	}
	sourceSummary, err := source.Summary(ctx)
	if err != nil {
		failRun(err)
		return
	}
	destinationSummary, err := destination.Summary(ctx)
	if err != nil {
		failRun(err)
		return
	}
	collectionRecords, err := s.db.DAVCollections(ctx, migrationID, kind)
	if err != nil {
		failRun(err)
		return
	}
	sourceByPath := collectionMapByPath(sourceSummary.Collections)
	destinationByName := collectionMapByName(destinationSummary.Collections)
	for _, collection := range collectionRecords {
		if !collection.Enabled {
			continue
		}
		if !waitRunning(ctx, control) {
			break
		}
		sourceCollection := sourceByPath[cleanPath(collection.SourcePath)]
		if sourceCollection.Path == "" {
			failRun(fmt.Errorf("source collection %q no longer exists", collection.SourceName))
			return
		}
		destinationCollection := findDestinationCollection(collection, destinationSummary.Collections, destinationByName)
		if destinationCollection.Path == "" {
			emit(collection.SourceName, "", domain.MigrationRunning)
			createErr := destination.CreateCollection(ctx, sourceCollection, collection.DestinationPath, collection.DestinationName)
			// Re-read after CREATE even on an error. A second client may have
			// created the same collection between inventory and write.
			refreshed, summaryErr := destination.Summary(ctx)
			if summaryErr != nil {
				if createErr != nil {
					failRun(fmt.Errorf("create destination collection %q: %w", collection.DestinationName, createErr))
				} else {
					failRun(summaryErr)
				}
				return
			}
			destinationSummary = refreshed
			destinationByName = collectionMapByName(destinationSummary.Collections)
			destinationCollection = findDestinationCollection(collection, destinationSummary.Collections, destinationByName)
			if destinationCollection.Path == "" {
				if createErr != nil {
					failRun(fmt.Errorf("create destination collection %q: %w", collection.DestinationName, createErr))
					return
				}
				failRun(fmt.Errorf("newly created destination collection %q was not found", collection.DestinationName))
				return
			}
			if err := destination.Probe(ctx, destinationCollection.Path); err != nil {
				failRun(err)
				return
			}
			_ = s.db.UpdateDAVCollectionDestination(ctx, collection.ID, destinationCollection.Path, destinationCollection.Name)
			collection.DestinationPath = destinationCollection.Path
		}
		if err := s.syncCollection(ctx, migrationID, request, options, mode, control, collection, sourceCollection, destinationCollection, source, destination, &total, &done, &failed, &totalBytes, &doneBytes, &runTotal, &runDone, &runPhase, &runIndeterminate, emit); err != nil {
			if ctx.Err() != nil {
				break
			}
			failRun(err)
			return
		}
	}
	if ctx.Err() != nil {
		_ = s.db.MarkService(context.Background(), migrationID, kind, domain.MigrationCancelled, "Cancelled by user")
		emit("", "", domain.MigrationCancelled)
		return
	}
	conflicts, _ := s.db.Conflicts(ctx, migrationID)
	unresolved := 0
	for _, conflict := range conflicts {
		if conflict.Kind == kind && conflict.Resolution == "" {
			unresolved++
		}
	}
	state := domain.MigrationCompleted
	if failed.Load() > 0 || unresolved > 0 {
		state = domain.MigrationCompletedWithErrors
	}
	runDone.Store(runTotal.Load())
	_ = s.db.MarkService(context.Background(), migrationID, kind, state, "")
	emit("", "", state)
}

func (s *Service) syncCollection(ctx context.Context, migrationID int64, request domain.DAVServiceRequest, options domain.TransferOptions, mode string, control *runControl, collection database.DAVCollectionRecord, sourceCollection, destinationCollection domain.DAVCollection, source, destination dav.Client, total, done, failed, totalBytes, doneBytes, runTotal, runDone *atomic.Int64, runPhase *atomic.Value, runIndeterminate *atomic.Bool, emit func(string, string, domain.MigrationState)) error {
	if err := s.db.ResetDAVInventory(ctx, collection.ID); err != nil {
		return err
	}
	targetInventory, err := destination.Inventory(ctx, destinationCollection.Path, "", 500)
	if err != nil {
		return fmt.Errorf("inventory destination collection %q: %w", destinationCollection.Name, err)
	}
	targets := make(map[string]dav.ResourceInfo, len(targetInventory.Resources))
	for _, resource := range targetInventory.Resources {
		targets[cleanPath(resource.Href)] = resource
	}
	existing, err := s.db.DAVResources(ctx, migrationID, collection.ID)
	if err != nil {
		return err
	}
	runTotal.Add(int64(len(existing)))
	for _, resource := range existing {
		runDone.Add(1)
		if target, ok := targets[cleanPath(resource.DestinationHref)]; ok && resource.DestinationHref != "" {
			_ = s.db.MarkDAVDestinationSeen(ctx, resource.ID)
			if resource.DestinationETag == "" {
				_ = s.db.UpdateDAVDestinationIdentity(ctx, resource.ID, target.Href, target.ETag)
			}
		}
	}
	if err := s.db.RequeueMissingDAVTargets(ctx, migrationID, collection.ID); err != nil {
		return err
	}
	sourceToken := ""
	if mode == "reconcile" {
		sourceToken = collection.SourceSyncToken
	}
	sourceInventory, err := source.Inventory(ctx, sourceCollection.Path, sourceToken, 500)
	if err != nil && sourceToken != "" && dav.IsSyncTokenInvalid(err) {
		sourceInventory, err = source.Inventory(ctx, sourceCollection.Path, "", 500)
	}
	if err != nil {
		return fmt.Errorf("inventory source collection %q: %w", sourceCollection.Name, err)
	}
	runPhase.Store("Delta sync")
	if mode != "reconcile" {
		runPhase.Store("Transfer")
	}
	runIndeterminate.Store(false)
	emit(collection.SourceName, "", domain.MigrationRunning)
	if !sourceInventory.Delta {
		total.Store(max(total.Load(), int64(len(sourceInventory.Resources))))
		var bytes int64
		for _, resource := range sourceInventory.Resources {
			bytes += resource.Size
		}
		totalBytes.Store(max(totalBytes.Load(), bytes))
		_ = s.db.UpdateServiceCurrent(ctx, migrationID, request.Kind, collection.SourceName, total.Load(), totalBytes.Load())
	}
	for _, resource := range sourceInventory.Resources {
		if _, err := s.db.UpsertDAVResource(ctx, migrationID, collection.ID, request.Kind, resource.Href, "", resource.ETag, resource.Size); err != nil {
			return err
		}
	}
	if err := s.db.SetDAVSyncToken(ctx, collection.ID, sourceInventory.SyncToken, targetInventory.SyncToken); err != nil {
		return err
	}
	resources, err := s.db.DAVResources(ctx, migrationID, collection.ID)
	if err != nil {
		return err
	}
	runTotal.Add(int64(len(resources)))
	for _, resource := range resources {
		if !waitRunning(ctx, control) {
			return ctx.Err()
		}
		runDone.Add(1)
		currentTarget, targetExists := targets[cleanPath(resource.DestinationHref)]
		repaired := resource.DestinationHref != "" && !targetExists
		resolution, _ := s.db.ConflictResolution(ctx, resource.ID)
		if targetExists && resource.DestinationETag != "" && currentTarget.ETag != resource.DestinationETag && resolution == "" {
			_ = s.db.AddConflict(ctx, migrationID, resource.ID, request.Kind, resource.SourceETag, currentTarget.ETag)
			_ = s.db.FailDAVResource(ctx, migrationID, resource.ID, domain.MessageSkipped, "Destination object changed after the migration")
			continue
		}
		if resolution == "destination" {
			_ = s.db.FailDAVResource(ctx, migrationID, resource.ID, domain.MessageSkipped, "Destination version was kept")
			continue
		}
		if resource.State == domain.MessageCopied || resource.State == domain.MessageSkipped {
			continue
		}
		emit(collection.SourceName+" · "+path.Base(resource.SourceHref), "", domain.MigrationRunning)
		if err := s.transferResource(ctx, migrationID, request, options, resource, sourceCollection, destinationCollection, source, destination, currentTarget, targetExists, repaired); err != nil {
			message := security.RedactError(err.Error(), request.Source.Password, request.Destination.Password)
			_ = s.db.FailDAVResource(context.Background(), migrationID, resource.ID, domain.MessageFailed, message)
			failed.Add(1)
			emit(collection.SourceName, message, domain.MigrationRunning)
			continue
		}
		done.Add(1)
		doneBytes.Add(resource.Size)
	}
	return nil
}

func (s *Service) transferResource(ctx context.Context, migrationID int64, request domain.DAVServiceRequest, options domain.TransferOptions, resource domain.DAVResourceState, sourceCollection, destinationCollection domain.DAVCollection, source, destination dav.Client, currentTarget dav.ResourceInfo, targetExists, repaired bool) error {
	if err := s.db.BeginDAVResource(ctx, resource.ID); err != nil {
		return err
	}
	var lastErr error
	for attempt := 0; attempt < max(1, options.MaximumRetries); attempt++ {
		if attempt > 0 {
			delay := retry.Backoff(attempt - 1)
			if httpErr, ok := lastErr.(*dav.HTTPError); ok && httpErr.RetryAfter > delay {
				delay = httpErr.RetryAfter
			}
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
		fetched, err := source.Get(ctx, resource.SourceHref, sourceCollection.MaxResourceSize)
		if err != nil {
			lastErr = err
			if !transient(err) {
				break
			}
			continue
		}
		data, readErr := io.ReadAll(fetched.Body)
		closeErr := fetched.Body.Close()
		if readErr != nil {
			lastErr = readErr
			continue
		}
		if closeErr != nil {
			lastErr = closeErr
			continue
		}
		transformed, err := dav.Transform(request.Kind, data, destinationCollection)
		if err != nil {
			return err
		}
		if err := s.db.UpdateDAVSourceIdentity(ctx, resource.ID, transformed.UID, fetched.ETag); err != nil {
			return err
		}
		for _, warning := range transformed.Warnings {
			_ = s.db.AddConversionWarning(ctx, migrationID, resource.ID, request.Kind, warning.Code, warning.Message)
		}
		sum := sha256.Sum256(transformed.Data)
		targetHref := resource.DestinationHref
		if targetHref == "" {
			targetHref = destinationHref(destinationCollection.Path, resource.SourceHref, request.Kind)
		}
		putOptions := dav.PutOptions{IfNoneMatch: !targetExists}
		if targetExists {
			putOptions.IfMatch = currentTarget.ETag
		}
		created, err := destination.Put(ctx, targetHref, transformed.ContentType, bytes.NewReader(transformed.Data), int64(len(transformed.Data)), putOptions)
		if err != nil {
			if dav.IsPreconditionFailed(err) {
				_ = s.db.AddConflict(ctx, migrationID, resource.ID, request.Kind, fetched.ETag, currentTarget.ETag)
				return fmt.Errorf("destination object was changed concurrently: %w", err)
			}
			lastErr = err
			if !transient(err) {
				break
			}
			continue
		}
		return s.db.CompleteDAVResource(ctx, migrationID, resource.ID, hex.EncodeToString(sum[:]), created.Href, created.ETag, int64(len(transformed.Data)), repaired)
	}
	return lastErr
}

func transient(err error) bool {
	if retry.IsTransient(err) {
		return true
	}
	var httpErr *dav.HTTPError
	return errors.As(err, &httpErr) && (httpErr.StatusCode == 408 || httpErr.StatusCode == 425 || httpErr.StatusCode == 429 || httpErr.StatusCode >= 500)
}

func destinationHref(collectionPath, sourceHref string, kind domain.ServiceKind) string {
	name := path.Base(strings.TrimRight(sourceHref, "/"))
	extension := ".ics"
	if kind == domain.ServiceContacts {
		extension = ".vcf"
	}
	if path.Ext(name) == "" {
		name += extension
	}
	return strings.TrimRight(collectionPath, "/") + "/" + name
}

func collectionMapByPath(collections []domain.DAVCollection) map[string]domain.DAVCollection {
	result := make(map[string]domain.DAVCollection, len(collections))
	for _, collection := range collections {
		result[cleanPath(collection.Path)] = collection
	}
	return result
}

func collectionMapByName(collections []domain.DAVCollection) map[string]domain.DAVCollection {
	result := make(map[string]domain.DAVCollection, len(collections))
	for _, collection := range collections {
		result[strings.ToLower(strings.TrimSpace(collection.Name))] = collection
	}
	return result
}

func findDestinationCollection(record database.DAVCollectionRecord, collections []domain.DAVCollection, byName map[string]domain.DAVCollection) domain.DAVCollection {
	for _, collection := range collections {
		if record.DestinationPath != "" && cleanPath(collection.Path) == cleanPath(record.DestinationPath) {
			return collection
		}
	}
	return byName[strings.ToLower(strings.TrimSpace(record.DestinationName))]
}

func cleanPath(value string) string { return strings.TrimRight(value, "/") }

func waitRunning(ctx context.Context, control *runControl) bool {
	for control.paused.Load() {
		select {
		case <-ctx.Done():
			return false
		case <-control.notify:
		case <-time.After(time.Second):
		}
	}
	return ctx.Err() == nil
}

func SortedKinds(request domain.StartJobRequest) []domain.ServiceKind {
	var kinds []domain.ServiceKind
	if request.MailEnabled {
		kinds = append(kinds, domain.ServiceMail)
	}
	if request.Calendar.Enabled {
		kinds = append(kinds, domain.ServiceCalendar)
	}
	if request.Contacts.Enabled {
		kinds = append(kinds, domain.ServiceContacts)
	}
	sort.Slice(kinds, func(i, j int) bool { return kinds[i] < kinds[j] })
	return kinds
}
