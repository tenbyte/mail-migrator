package migration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tenbyte/mail-migrator/internal/database"
	"github.com/tenbyte/mail-migrator/internal/domain"
	"github.com/tenbyte/mail-migrator/internal/folders"
	"github.com/tenbyte/mail-migrator/internal/mailimap"
	"github.com/tenbyte/mail-migrator/internal/retry"
	"github.com/tenbyte/mail-migrator/internal/security"
)

type EventSink func(domain.Progress)

type Service struct {
	db      *database.DB
	factory mailimap.Factory
	events  EventSink
	mu      sync.Mutex
	runs    map[int64]*runControl
}

type runControl struct {
	cancel context.CancelFunc
	paused atomic.Bool
	notify chan struct{}
}

type runFailure struct {
	mu      sync.Mutex
	message string
}

func (failure *runFailure) set(message string) {
	failure.mu.Lock()
	failure.message = message
	failure.mu.Unlock()
}

func (failure *runFailure) get() string {
	failure.mu.Lock()
	defer failure.mu.Unlock()
	return failure.message
}

func New(db *database.DB, factory mailimap.Factory, events EventSink) *Service {
	return &Service{db: db, factory: factory, events: events, runs: make(map[int64]*runControl)}
}

func (s *Service) Active() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.runs) > 0
}

func (s *Service) Inspect(ctx context.Context, account domain.AccountConfig) (domain.ServerSummary, error) {
	client, err := s.factory.Connect(ctx, account, 15*time.Second, 90*time.Second)
	if err != nil {
		return domain.ServerSummary{}, err
	}
	defer client.Close()
	caps, err := client.Capabilities(ctx)
	if err != nil {
		return domain.ServerSummary{}, err
	}
	mailboxes, err := client.ListMailboxes(ctx)
	if err != nil {
		return domain.ServerSummary{}, err
	}
	mailboxes, err = fillMailboxSizes(ctx, client, mailboxes)
	if err != nil {
		return domain.ServerSummary{}, err
	}
	summary := summarize(account.Host, caps, mailboxes)
	if limits, limitErr := client.Limits(ctx, limitMailbox(mailboxes)); limitErr == nil {
		summary.AppendLimit = limits.AppendLimit
		summary.QuotaAvailableBytes = limits.QuotaAvailableBytes
		summary.QuotaUsedBytes = limits.QuotaUsedBytes
	} else {
		summary.Warnings = append(summary.Warnings, "IMAP quota could not be read; the transfer still checks server errors for each message.")
	}
	return summary, nil
}

func (s *Service) Preflight(ctx context.Context, source, destination domain.AccountConfig) (domain.PreflightResult, error) {
	type sideResult struct {
		source   bool
		summary  domain.ServerSummary
		keywords []domain.MailKeyword
		err      error
	}
	results := make(chan sideResult, 2)
	check := func(account domain.AccountConfig, sourceSide bool) {
		client, err := s.factory.Connect(ctx, account, 15*time.Second, 90*time.Second)
		if err != nil {
			results <- sideResult{source: sourceSide, err: err}
			return
		}
		defer client.Close()
		caps, err := client.Capabilities(ctx)
		if err != nil {
			results <- sideResult{source: sourceSide, err: err}
			return
		}
		mailboxes, err := client.ListMailboxes(ctx)
		if err != nil {
			results <- sideResult{source: sourceSide, err: err}
			return
		}
		if sourceSide {
			mailboxes, err = fillMailboxSizes(ctx, client, mailboxes)
			if err != nil {
				results <- sideResult{source: true, err: err}
				return
			}
			keywords, keywordErr := inventorySourceKeywords(ctx, client, mailboxes)
			if keywordErr != nil {
				results <- sideResult{source: true, err: keywordErr}
				return
			}
			summary := summarize(account.Host, caps, mailboxes)
			if limits, limitErr := client.Limits(ctx, limitMailbox(mailboxes)); limitErr == nil {
				summary.AppendLimit = limits.AppendLimit
				summary.QuotaAvailableBytes = limits.QuotaAvailableBytes
				summary.QuotaUsedBytes = limits.QuotaUsedBytes
			} else {
				summary.Warnings = append(summary.Warnings, "IMAP quota could not be read; the transfer still checks server errors for each message.")
			}
			results <- sideResult{source: true, summary: summary, keywords: keywords}
			return
		}
		summary := summarize(account.Host, caps, mailboxes)
		if limits, limitErr := client.Limits(ctx, limitMailbox(mailboxes)); limitErr == nil {
			summary.AppendLimit = limits.AppendLimit
			summary.QuotaAvailableBytes = limits.QuotaAvailableBytes
			summary.QuotaUsedBytes = limits.QuotaUsedBytes
		} else {
			summary.Warnings = append(summary.Warnings, "IMAP quota could not be read; the transfer still checks server errors for each message.")
		}
		results <- sideResult{source: sourceSide, summary: summary}
	}
	go check(source, true)
	go check(destination, false)
	first, second := <-results, <-results
	if first.err != nil {
		return domain.PreflightResult{}, first.err
	}
	if second.err != nil {
		return domain.PreflightResult{}, second.err
	}
	// Results can arrive in either order.
	var src, dst domain.ServerSummary
	var keywords []domain.MailKeyword
	if first.source {
		src, dst = first.summary, second.summary
		keywords = first.keywords
	} else {
		src, dst = second.summary, first.summary
		keywords = second.keywords
	}
	mappings := folders.Recommend(src.Mailboxes, dst.Mailboxes)
	for _, mapping := range mappings {
		if mapping.Enabled && strings.EqualFold(strings.TrimSpace(mapping.DestinationName), "INBOX") && !mapping.DestinationExists {
			return domain.PreflightResult{}, errors.New("the destination server does not report a writable INBOX folder; open the destination mailbox with the provider once and run preflight again")
		}
	}
	warnings := append([]string{}, src.Warnings...)
	warnings = append(warnings, dst.Warnings...)
	if dst.AppendLimit > 0 {
		warnings = append(warnings, fmt.Sprintf("The destination limits individual messages to %d bytes; each message is checked before APPEND.", dst.AppendLimit))
	}
	if dst.QuotaAvailableBytes > 0 && src.Bytes > dst.QuotaAvailableBytes {
		warnings = append(warnings, "The destination reports less free storage than the selected source data requires.")
	}
	return domain.PreflightResult{Source: src, Destination: dst, Mappings: mappings, Keywords: keywords, Warnings: warnings}, nil
}

func inventorySourceKeywords(ctx context.Context, client mailimap.Client, mailboxes []domain.Mailbox) ([]domain.MailKeyword, error) {
	byName := make(map[string]*domain.MailKeyword)
	for _, mailbox := range mailboxes {
		if !mailbox.Selectable || mailbox.Messages == 0 {
			continue
		}
		counts, err := client.ListMessageKeywords(ctx, mailbox.Name)
		if err != nil {
			return nil, fmt.Errorf("inventory tags in folder %q: %w", mailbox.Name, err)
		}
		for _, count := range counts {
			key := strings.ToLower(count.Name)
			item := byName[key]
			if item == nil {
				item = &domain.MailKeyword{Name: count.Name, Occurrences: make(map[string]int64)}
				byName[key] = item
			}
			item.Occurrences[mailbox.Name] += count.Messages
		}
	}
	result := make([]domain.MailKeyword, 0, len(byName))
	for _, item := range byName {
		result = append(result, *item)
	}
	sort.Slice(result, func(i, j int) bool { return strings.ToLower(result[i].Name) < strings.ToLower(result[j].Name) })
	return result, nil
}

func fillMailboxSizes(ctx context.Context, client mailimap.Client, mailboxes []domain.Mailbox) ([]domain.Mailbox, error) {
	for index := range mailboxes {
		mailbox := &mailboxes[index]
		if !mailbox.Selectable || mailbox.SizeKnown {
			continue
		}
		metadata, err := client.ListMessageMetadata(ctx, mailbox.Name)
		if err != nil {
			return nil, fmt.Errorf("determine size of folder %q: %w", mailbox.Name, err)
		}
		var size int64
		for _, message := range metadata {
			if !message.SizeKnown || message.Size < 0 {
				return nil, fmt.Errorf("size of folder %q is incomplete", mailbox.Name)
			}
			size += message.Size
		}
		mailbox.Size = size
		mailbox.SizeKnown = true
	}
	return mailboxes, nil
}

func limitMailbox(mailboxes []domain.Mailbox) string {
	for _, mailbox := range mailboxes {
		if mailbox.Selectable && strings.EqualFold(mailbox.Name, "INBOX") {
			return mailbox.Name
		}
	}
	for _, mailbox := range mailboxes {
		if mailbox.Selectable {
			return mailbox.Name
		}
	}
	return ""
}

func summarize(host string, caps []string, mailboxes []domain.Mailbox) domain.ServerSummary {
	s := domain.ServerSummary{Connected: true, Host: host, Capabilities: caps, Mailboxes: mailboxes, FolderCount: len(mailboxes)}
	for _, box := range mailboxes {
		s.Messages += int64(box.Messages)
		s.Bytes += box.Size
	}
	for _, capability := range caps {
		upper := strings.ToUpper(capability)
		if upper == "UIDPLUS" || upper == "IMAP4REV2" {
			s.UIDPlus = true
		}
		if strings.HasPrefix(upper, "APPENDLIMIT=") {
			fmt.Sscanf(strings.TrimPrefix(upper, "APPENDLIMIT="), "%d", &s.AppendLimit)
		}
	}
	if strings.Contains(strings.ToLower(host), "gmail") || containsMailbox(mailboxes, "[Gmail]") {
		s.Warnings = append(s.Warnings, "Gmail label semantics can create physical duplicates on classic IMAP destinations. Gmail support is experimental.")
	}
	return s
}

func (s *Service) Start(parent context.Context, request domain.StartRequest) (int64, error) {
	if request.Options.Concurrency == 0 {
		request.Options = domain.DefaultTransferOptions()
	}
	request.Options.Concurrency = max(1, min(8, request.Options.Concurrency))
	request.Options.MaximumRetries = max(1, min(20, request.Options.MaximumRetries))
	if request.Options.VerificationMode == "" {
		if request.Options.VerifyAfter {
			request.Options.VerificationMode = domain.VerificationFullHash
		} else {
			request.Options.VerificationMode = domain.VerificationMetadata
		}
	}
	id := request.MigrationID
	var err error
	if id == 0 {
		id, err = s.db.CreateMigration(parent, request)
		if err != nil {
			return 0, err
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	control := &runControl{cancel: cancel, notify: make(chan struct{}, 1)}
	s.mu.Lock()
	if _, exists := s.runs[id]; exists {
		s.mu.Unlock()
		cancel()
		return 0, errors.New("migration is already running")
	}
	s.runs[id] = control
	s.mu.Unlock()
	go func() {
		defer func() { s.mu.Lock(); delete(s.runs, id); s.mu.Unlock() }()
		s.run(ctx, id, request, control)
	}()
	return id, nil
}

func (s *Service) Pause(id int64) error {
	s.mu.Lock()
	run := s.runs[id]
	s.mu.Unlock()
	if run == nil {
		return errors.New("migration is not running")
	}
	run.paused.Store(true)
	return s.db.MarkMigration(context.Background(), id, domain.MigrationPaused, "")
}
func (s *Service) Resume(id int64) error {
	s.mu.Lock()
	run := s.runs[id]
	s.mu.Unlock()
	if run == nil {
		return errors.New("migration is not running")
	}
	run.paused.Store(false)
	select {
	case run.notify <- struct{}{}:
	default:
	}
	return s.db.MarkMigration(context.Background(), id, domain.MigrationRunning, "")
}
func (s *Service) Cancel(id int64) error {
	s.mu.Lock()
	run := s.runs[id]
	s.mu.Unlock()
	if run == nil {
		return errors.New("migration is not running")
	}
	run.cancel()
	return nil
}

func (s *Service) run(ctx context.Context, id int64, request domain.StartRequest, control *runControl) {
	if err := s.db.MarkRunning(ctx, id); err != nil {
		return
	}
	started := time.Now()
	var copiedBytes, copiedMessages, failedMessages atomic.Int64
	var runItemsTotal, runItemsDone atomic.Int64
	var runPhase atomic.Value
	var runIndeterminate atomic.Bool
	runPhase.Store("Transfer")
	if request.Mode == "reconcile" {
		runPhase.Store("Inventory")
		runIndeterminate.Store(true)
	}
	lastFailure := &runFailure{}
	totalMessages, totalBytes := int64(0), int64(0)
	for _, mapping := range request.Mappings {
		if mapping.Enabled {
			totalMessages += int64(mapping.Source.Messages)
			totalBytes += mapping.Source.Size
		}
	}
	if previous, previousErr := s.db.RecentByID(ctx, id); previousErr == nil {
		copiedBytes.Store(previous.BytesCopied)
		copiedMessages.Store(previous.MessagesCopied)
		failedMessages.Store(previous.MessagesFailed)
		if totalMessages == 0 {
			totalMessages = previous.MessagesTotal
		}
		if totalBytes == 0 {
			totalBytes = previous.BytesTotal
		}
	}
	if request.Mode != "reconcile" {
		runItemsTotal.Store(totalMessages)
	}
	emit := func(folder string, uid uint32, state domain.MigrationState, lastErr string) {
		if s.events == nil {
			return
		}
		elapsed := time.Since(started).Seconds()
		speed := float64(0)
		if elapsed > 0 {
			speed = float64(copiedBytes.Load()) / elapsed
		}
		s.events(domain.Progress{MigrationID: id, Service: domain.ServiceMail, State: state, CurrentFolder: folder, CurrentUID: uid, MessagesTotal: totalMessages, MessagesCopied: copiedMessages.Load(), MessagesFailed: failedMessages.Load(), BytesTotal: totalBytes, BytesCopied: copiedBytes.Load(), BytesPerSecond: speed, StartedAt: started, LastError: lastErr, RunMode: request.Mode, RunPhase: runPhase.Load().(string), RunItemsTotal: runItemsTotal.Load(), RunItemsDone: runItemsDone.Load(), RunIndeterminate: runIndeterminate.Load()})
	}
	emit("Preparing operation", 0, domain.MigrationRunning, "")
	if err := s.createFolders(ctx, request); err != nil {
		message := security.RedactError(err.Error(), request.Source.Password, request.Destination.Password)
		_ = s.db.AddServiceError(context.Background(), id, 0, domain.ServiceMail, "TB-MAIL-FOLDER-CREATE", message)
		_ = s.db.MarkMigration(context.Background(), id, domain.MigrationFailed, message)
		emit("", 0, domain.MigrationFailed, message)
		return
	}
	records, err := s.db.Folders(ctx, id)
	if err != nil {
		message := security.SanitizeLogValue(err.Error())
		_ = s.db.AddServiceError(context.Background(), id, 0, domain.ServiceMail, "TB-MAIL-STATE-FOLDERS", message)
		_ = s.db.MarkMigration(context.Background(), id, domain.MigrationFailed, message)
		return
	}
	if request.Mode == "reconcile" {
		var tracked int64
		for _, folder := range records {
			if !folder.Enabled || folder.SourceUIDValidity == 0 {
				continue
			}
			items, copiedErr := s.db.CopiedMessages(ctx, id, folder.ID, folder.SourceUIDValidity)
			if copiedErr == nil {
				tracked += int64(len(items))
			}
		}
		runItemsTotal.Store(tracked + 1)
		runPhase.Store("Delta sync")
		runIndeterminate.Store(false)
		emit("Reconciling source and destination", 0, domain.MigrationRunning, "")
	}
	jobs := make(chan database.FolderRecord)
	duplicateIndexes := &duplicateIndexCache{indexes: make(map[string]*duplicateIndex)}
	var wg sync.WaitGroup
	for worker := 0; worker < request.Options.Concurrency; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.worker(ctx, id, request, control, jobs, duplicateIndexes, &copiedBytes, &copiedMessages, &failedMessages, &runItemsTotal, &runItemsDone, lastFailure, emit)
		}()
	}
dispatch:
	for _, folder := range records {
		if !folder.Enabled {
			continue
		}
		select {
		case jobs <- folder:
		case <-ctx.Done():
			break dispatch
		}
	}
	close(jobs)
	wg.Wait()
	if ctx.Err() != nil {
		_ = s.db.RecoverMigrationMessages(context.Background(), id)
		_ = s.db.MarkMigration(context.Background(), id, domain.MigrationCancelled, "cancelled by user")
		emit("", 0, domain.MigrationCancelled, "")
		return
	}
	state := domain.MigrationCompleted
	if failedMessages.Load() > 0 {
		state = domain.MigrationCompletedWithErrors
	}
	message := lastFailure.get()
	runItemsDone.Store(runItemsTotal.Load())
	_ = s.db.MarkMigration(context.Background(), id, state, message)
	emit("", 0, state, message)
}

func (s *Service) createFolders(ctx context.Context, request domain.StartRequest) error {
	dest, err := s.factory.Connect(ctx, request.Destination, time.Duration(request.Options.ConnectionTimeout)*time.Second, time.Duration(request.Options.StallTimeout)*time.Second)
	if err != nil {
		return err
	}
	defer dest.Close()
	mailboxes, err := dest.ListMailboxes(ctx)
	if err != nil {
		return fmt.Errorf("check destination folders before migration: %w", err)
	}
	existing := mailboxNames(mailboxes)
	var names []string
	for _, mapping := range request.Mappings {
		if !mapping.Enabled {
			continue
		}
		delimiter := mapping.DestinationDelimiter
		if delimiter == 0 {
			delimiter = '/'
		}
		parts := strings.Split(mapping.DestinationName, string(delimiter))
		for i := range parts {
			name := strings.Join(parts[:i+1], string(delimiter))
			if folders.SafeName(name) {
				names = append(names, name)
			}
		}
	}
	sort.SliceStable(names, func(i, j int) bool { return len(names[i]) < len(names[j]) })
	seen := map[string]bool{}
	for _, name := range names {
		key := mailboxKey(name)
		if seen[key] {
			continue
		}
		seen[key] = true
		if _, exists := existing[key]; exists {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(name), "INBOX") {
			return errors.New("the destination server does not report an existing INBOX folder; INBOX cannot be created manually, so open the destination mailbox with the provider once and run preflight again")
		}
		if err := dest.CreateMailbox(ctx, name); err != nil {
			// Another client may have created the mailbox between our inventory and
			// CREATE. Re-read the server state before treating the response as fatal.
			current, listErr := dest.ListMailboxes(ctx)
			if listErr == nil {
				existing = mailboxNames(current)
				if _, exists := existing[key]; exists {
					continue
				}
			}
			return fmt.Errorf("create destination folder %q: %w", name, err)
		}
		existing[key] = name
	}
	return nil
}

func mailboxKey(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

func mailboxNames(mailboxes []domain.Mailbox) map[string]string {
	names := make(map[string]string, len(mailboxes))
	for _, mailbox := range mailboxes {
		names[mailboxKey(mailbox.Name)] = mailbox.Name
	}
	return names
}

type emitFunc func(string, uint32, domain.MigrationState, string)

type mailIssueError struct {
	state domain.MessageState
	code  string
	text  string
}

func (e *mailIssueError) Error() string { return "[" + e.code + "] " + e.text }

func issue(state domain.MessageState, code, text string) error {
	return &mailIssueError{state: state, code: code, text: text}
}

func hashSelectedMessage(ctx context.Context, client mailimap.Client, uid uint32, expectedSize int64, sizeKnown bool) (string, int64, error) {
	var digest string
	var literalSize int64
	err := client.StreamMessage(ctx, uid, func(reader io.Reader, size int64) error {
		literalSize = size
		if size == 0 && !sizeKnown {
			return issue(domain.MessageQuarantined, "TB-MAIL-SOURCE-EMPTY", "The source returned an empty message.")
		}
		if sizeKnown && size != expectedSize {
			return issue(domain.MessageQuarantined, "TB-MAIL-SOURCE-SIZE-MISMATCH", fmt.Sprintf("Literal size %d does not match RFC822.SIZE %d.", size, expectedSize))
		}
		hash := sha256.New()
		written, err := io.CopyBuffer(hash, reader, make([]byte, 64*1024))
		if err != nil {
			return err
		}
		if written != size {
			return issue(domain.MessageQuarantined, "TB-MAIL-SOURCE-SIZE-MISMATCH", fmt.Sprintf("The source returned %d instead of %d bytes.", written, size))
		}
		digest = hex.EncodeToString(hash.Sum(nil))
		return nil
	})
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "body not returned") || strings.Contains(strings.ToLower(err.Error()), "literal") {
			var typed *mailIssueError
			if !errors.As(err, &typed) {
				return "", literalSize, issue(domain.MessageQuarantined, "TB-MAIL-SOURCE-LITERAL-MISSING", "The server did not return a complete raw body.")
			}
		}
		return "", literalSize, err
	}
	return digest, literalSize, nil
}

func verifyDestinationMessage(ctx context.Context, client mailimap.Client, uid uint32, expectedSize int64, sourceSHA string, fullHash bool) (string, error) {
	meta, err := client.FetchMetadata(ctx, uid)
	if err != nil {
		return "", issue(domain.MessageFailed, "TB-MAIL-VERIFY-MISSING", "The newly stored destination message could not be opened again.")
	}
	if meta.SizeKnown && meta.Size != expectedSize {
		return "", issue(domain.MessageFailed, "TB-MAIL-VERIFY-SIZE", fmt.Sprintf("The destination reports %d instead of %d bytes.", meta.Size, expectedSize))
	}
	if !fullHash {
		return "", nil
	}
	destinationSHA, literalSize, err := hashSelectedMessage(ctx, client, uid, expectedSize, true)
	if err != nil {
		return "", issue(domain.MessageFailed, "TB-MAIL-VERIFY-READ", "The destination message could not be read completely for verification.")
	}
	if literalSize != expectedSize {
		return destinationSHA, issue(domain.MessageFailed, "TB-MAIL-VERIFY-SIZE", fmt.Sprintf("The destination message contains %d instead of %d bytes.", literalSize, expectedSize))
	}
	if sourceSHA == "" || destinationSHA != sourceSHA {
		return destinationSHA, issue(domain.MessageFailed, "TB-MAIL-VERIFY-HASH", "The SHA-256 hash of the destination message does not match the source.")
	}
	return destinationSHA, nil
}

func matchingDestinationUIDs(ctx context.Context, client mailimap.Client, after uint32, expectedSize int64, sourceSHA string) ([]uint32, error) {
	uids, err := client.SearchUIDs(ctx, after)
	if err != nil {
		return nil, err
	}
	matches := make([]uint32, 0, 1)
	for _, uid := range uids {
		meta, err := client.FetchMetadata(ctx, uid)
		if err != nil || (meta.SizeKnown && meta.Size != expectedSize) {
			continue
		}
		digest, size, err := hashSelectedMessage(ctx, client, uid, expectedSize, true)
		if err == nil && size == expectedSize && digest == sourceSHA {
			matches = append(matches, uid)
		}
	}
	return matches, nil
}

type duplicateCandidate struct {
	uid    uint32
	size   int64
	digest string
	hashed bool
	used   bool
}

type duplicateIndex struct {
	mu     sync.Mutex
	bySize map[int64][]*duplicateCandidate
}

type duplicateIndexCache struct {
	mu      sync.Mutex
	indexes map[string]*duplicateIndex
}

func (cache *duplicateIndexCache) get(ctx context.Context, client mailimap.Client, mailbox string) (*duplicateIndex, error) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	key := mailboxKey(mailbox)
	if index := cache.indexes[key]; index != nil {
		return index, nil
	}
	index, err := newDuplicateIndex(ctx, client, mailbox)
	if err != nil {
		return nil, err
	}
	cache.indexes[key] = index
	return index, nil
}

func newDuplicateIndex(ctx context.Context, client mailimap.Client, mailbox string) (*duplicateIndex, error) {
	metadata, err := client.ListMessageMetadata(ctx, mailbox)
	if err != nil {
		return nil, fmt.Errorf("inventory destination folder %q for duplicate protection: %w", mailbox, err)
	}
	index := &duplicateIndex{bySize: make(map[int64][]*duplicateCandidate)}
	for _, message := range metadata {
		if !message.SizeKnown {
			return nil, fmt.Errorf("destination folder %q reports a message without a size", mailbox)
		}
		candidate := &duplicateCandidate{uid: message.UID, size: message.Size}
		index.bySize[message.Size] = append(index.bySize[message.Size], candidate)
	}
	return index, nil
}

func (index *duplicateIndex) consume(uid uint32) {
	if index == nil || uid == 0 {
		return
	}
	index.mu.Lock()
	defer index.mu.Unlock()
	for _, candidates := range index.bySize {
		for _, candidate := range candidates {
			if candidate.uid == uid {
				candidate.used = true
				return
			}
		}
	}
}

func (index *duplicateIndex) addConsumed(uid uint32, size int64, digest string) {
	if index == nil || uid == 0 {
		return
	}
	index.mu.Lock()
	defer index.mu.Unlock()
	index.bySize[size] = append(index.bySize[size], &duplicateCandidate{uid: uid, size: size, digest: digest, hashed: digest != "", used: true})
}

func (index *duplicateIndex) findExact(ctx context.Context, source, destination mailimap.Client, sourceUID uint32, metadata mailimap.MessageMetadata) (uint32, string, int64, bool, error) {
	if index == nil || !metadata.SizeKnown {
		return 0, "", metadata.Size, false, nil
	}
	index.mu.Lock()
	defer index.mu.Unlock()
	candidates := index.bySize[metadata.Size]
	available := false
	for _, candidate := range candidates {
		if !candidate.used {
			available = true
			break
		}
	}
	if !available {
		return 0, "", metadata.Size, false, nil
	}
	sourceSHA, sourceSize, err := hashSelectedMessage(ctx, source, sourceUID, metadata.Size, true)
	if err != nil {
		return 0, "", sourceSize, false, err
	}
	for _, candidate := range candidates {
		if candidate.used {
			continue
		}
		if !candidate.hashed {
			digest, size, hashErr := hashSelectedMessage(ctx, destination, candidate.uid, candidate.size, true)
			if hashErr != nil || size != candidate.size {
				if hashErr == nil {
					hashErr = fmt.Errorf("destination message UID %d returned %d instead of %d bytes", candidate.uid, size, candidate.size)
				}
				return 0, sourceSHA, sourceSize, false, fmt.Errorf("read destination message UID %d for duplicate protection: %v", candidate.uid, hashErr)
			}
			candidate.digest = digest
			candidate.hashed = true
		}
		if candidate.digest == sourceSHA {
			candidate.used = true
			return candidate.uid, sourceSHA, sourceSize, true, nil
		}
	}
	return 0, sourceSHA, sourceSize, false, nil
}

func uidBefore(next uint32) uint32 {
	if next > 1 {
		return next - 1
	}
	return 0
}

func isQuotaError(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "overquota") || strings.Contains(text, "quota") || strings.Contains(text, "disk full")
}

func isAppendLimitError(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "appendlimit") || strings.Contains(text, "too large") || strings.Contains(text, "toobig") || strings.Contains(text, "message size")
}

func (s *Service) recordFolderFailure(id int64, request domain.StartRequest, folderID int64, folderName, code string, err error, failedMessages *atomic.Int64, lastFailure *runFailure, emit emitFunc) {
	message := security.RedactError(err.Error(), request.Source.Password, request.Destination.Password)
	message = security.SanitizeLogValue(message)
	failedMessages.Add(1)
	lastFailure.set(message)
	_ = s.db.AddServiceError(context.Background(), id, folderID, domain.ServiceMail, code, message)
	emit(folderName, 0, domain.MigrationRunning, "["+code+"] "+message)
}

func (s *Service) worker(ctx context.Context, id int64, request domain.StartRequest, control *runControl, jobs <-chan database.FolderRecord, duplicateIndexes *duplicateIndexCache, copiedBytes, copiedMessages, failedMessages, runItemsTotal, runItemsDone *atomic.Int64, lastFailure *runFailure, emit emitFunc) {
	connect := func() (mailimap.Client, mailimap.Client, error) {
		timeout := time.Duration(request.Options.ConnectionTimeout) * time.Second
		stall := time.Duration(request.Options.StallTimeout) * time.Second
		src, err := s.factory.Connect(ctx, request.Source, timeout, stall)
		if err != nil {
			return nil, nil, err
		}
		dst, err := s.factory.Connect(ctx, request.Destination, timeout, stall)
		if err != nil {
			src.Close()
			return nil, nil, err
		}
		return src, dst, nil
	}
	src, dst, err := connect()
	if err != nil {
		s.recordFolderFailure(id, request, 0, "", "TB-MAIL-CONNECTION", err, failedMessages, lastFailure, emit)
		return
	}
	defer func() { src.Close(); dst.Close() }()
	for folder := range jobs {
		connectionUsable := true
		if !waitRunning(ctx, control) {
			return
		}
		uidValidity, _, _, err := src.SelectMailbox(ctx, folder.SourceName, true)
		if err != nil {
			s.recordFolderFailure(id, request, folder.ID, folder.SourceName, "TB-MAIL-SOURCE-SELECT", err, failedMessages, lastFailure, emit)
			continue
		}
		after := folder.LastSyncedUID
		if folder.SourceUIDValidity != 0 && folder.SourceUIDValidity != uidValidity {
			after = 0
			_ = s.db.SetFolderUIDValidity(ctx, folder.ID, uidValidity, true)
		} else {
			_ = s.db.SetFolderUIDValidity(ctx, folder.ID, uidValidity, false)
		}
		destinationUIDValidity, _, allowed, err := dst.SelectMailbox(ctx, folder.DestinationName, false)
		if err != nil {
			s.recordFolderFailure(id, request, folder.ID, folder.SourceName, "TB-MAIL-TARGET-SELECT", err, failedMessages, lastFailure, emit)
			continue
		}
		// Only read the advertised APPENDLIMIT on the transfer connection.
		// Some servers advertise QUOTA but return a malformed GETQUOTAROOT
		// response that fatally closes the protocol connection. Quota remains an
		// optional preflight hint and actual quota failures are handled on APPEND.
		limits, _ := dst.Limits(ctx, "")
		var duplicates *duplicateIndex
		if request.Options.DuplicateProtection {
			duplicates, err = duplicateIndexes.get(ctx, dst, folder.DestinationName)
			if err != nil {
				s.recordFolderFailure(id, request, folder.ID, folder.SourceName, "TB-MAIL-DUPLICATE-INVENTORY", err, failedMessages, lastFailure, emit)
				continue
			}
			tracked, trackedErr := s.db.CopiedMessages(ctx, id, folder.ID, uidValidity)
			if trackedErr != nil {
				s.recordFolderFailure(id, request, folder.ID, folder.SourceName, "TB-MAIL-STATE-COPIED", trackedErr, failedMessages, lastFailure, emit)
				continue
			}
			for _, record := range tracked {
				duplicates.consume(record.DestinationUID)
			}
			// The inventory uses a read-only SELECT. Restore the writable
			// destination selection before any transfer work begins.
			destinationUIDValidity, _, allowed, err = dst.SelectMailbox(ctx, folder.DestinationName, false)
			if err != nil {
				s.recordFolderFailure(id, request, folder.ID, folder.SourceName, "TB-MAIL-TARGET-RESELECT", err, failedMessages, lastFailure, emit)
				continue
			}
		}
		var sourceUIDs []uint32
		if request.Mode == "reconcile" {
			emit("Reconciliation: "+folder.SourceName, 0, domain.MigrationRunning, "")
			sourceUIDs, err = src.SearchUIDs(ctx, 0)
			if err == nil {
				err = s.reconcileFolder(ctx, id, request, folder, uidValidity, destinationUIDValidity, sourceUIDs, dst, copiedBytes, copiedMessages, runItemsDone)
			}
			// Candidate reconciliation may select the mailbox read-only. Restore
			// the writable destination selection and its permanent flags.
			if err == nil {
				destinationUIDValidity, _, allowed, err = dst.SelectMailbox(ctx, folder.DestinationName, false)
			}
		} else {
			sourceUIDs, err = src.SearchUIDs(ctx, after)
		}
		if err != nil {
			s.recordFolderFailure(id, request, folder.ID, folder.SourceName, "TB-MAIL-RECONCILE", err, failedMessages, lastFailure, emit)
			continue
		}
		_ = s.db.SetDestinationUIDValidity(ctx, folder.ID, destinationUIDValidity)
		uids := sourceUIDs
		if request.Mode == "reconcile" {
			uids = uidsAfter(sourceUIDs, after)
		}
		unfinished, unfinishedErr := s.db.UnfinishedUIDs(ctx, id, folder.ID, uidValidity)
		if unfinishedErr != nil {
			s.recordFolderFailure(id, request, folder.ID, folder.SourceName, "TB-MAIL-STATE-UNFINISHED", unfinishedErr, failedMessages, lastFailure, emit)
			continue
		}
		uids = mergeUIDs(unfinished, uids)
		if request.Mode == "reconcile" {
			runItemsTotal.Add(int64(len(uids)))
		}
		for _, uid := range uids {
			if !waitRunning(ctx, control) {
				return
			}
			runItemsDone.Add(1)
			emit(folder.SourceName, uid, domain.MigrationRunning, "")
			meta, err := src.FetchMetadata(ctx, uid)
			if err != nil {
				code := "TB-MAIL-SOURCE-METADATA"
				state := domain.MessageFailed
				lower := strings.ToLower(err.Error())
				if strings.Contains(lower, "not returned") || strings.Contains(lower, "not found") || strings.Contains(lower, "no such") {
					code, state = "TB-MAIL-SOURCE-GONE", domain.MessageSkipped
				}
				_ = s.db.RecordMessageIssue(ctx, id, folder.ID, uidValidity, uid, 0, "", time.Time{}, state, code, security.SanitizeLogValue(err.Error()))
				failedMessages.Add(1)
				emit(folder.SourceName, uid, domain.MigrationRunning, "["+code+"] The message could not be inventoried at the source.")
				continue
			}
			existing, _ := s.db.MessageTransfer(ctx, id, folder.ID, uidValidity, uid)
			forceEmpty := existing.PolicyOverride == string(domain.MailIssueTransferAnyway)
			if (existing.State == domain.MessageQuarantined || existing.State == domain.MessageFailed || existing.State == domain.MessageSkipped) && existing.PolicyOverride == "" {
				continue
			}
			if meta.SizeKnown && meta.Size == 0 && !forceEmpty {
				message := "The source reports RFC822.SIZE 0. The message was quarantined and not transferred."
				_ = s.db.RecordMessageIssue(ctx, id, folder.ID, uidValidity, uid, 0, "", meta.InternalDate, domain.MessageQuarantined, "TB-MAIL-SOURCE-EMPTY", message)
				failedMessages.Add(1)
				emit(folder.SourceName, uid, domain.MigrationRunning, "[TB-MAIL-SOURCE-EMPTY] "+message)
				continue
			}
			if existing.State == domain.MessageUnknown {
				sourceSHA := existing.SourceSHA
				sourceSize := meta.Size
				if sourceSHA == "" {
					var sourceErr error
					sourceSHA, sourceSize, sourceErr = hashSelectedMessage(ctx, src, uid, meta.Size, meta.SizeKnown)
					if sourceErr != nil {
						var typed *mailIssueError
						if errors.As(sourceErr, &typed) {
							_ = s.db.FailMessageCode(ctx, id, folder.ID, uid, typed.state, typed.code, typed.text)
						}
						emit(folder.SourceName, uid, domain.MigrationRunning, "[TB-MAIL-TARGET-APPEND-UNKNOWN] The message with an unknown outcome could not be checked safely against the destination.")
						continue
					}
					_ = s.db.SetMessageTransferIdentity(ctx, id, folder.ID, uid, sourceSHA, existing.PreAppendUIDNext)
					_ = s.db.FailMessageCode(ctx, id, folder.ID, uid, domain.MessageUnknown, "TB-MAIL-TARGET-APPEND-UNKNOWN", "The APPEND outcome continues to be checked only at the destination.")
				}
				matches, matchErr := matchingDestinationUIDs(ctx, dst, uidBefore(existing.PreAppendUIDNext), sourceSize, sourceSHA)
				if matchErr == nil && len(matches) == 1 {
					if err := s.db.CompleteVerifiedMessage(ctx, id, folder.ID, uidValidity, uid, matches[0], sourceSize, sourceSHA, sourceSHA, "verified"); err == nil {
						copiedMessages.Add(1)
						copiedBytes.Add(sourceSize)
						if failedMessages.Load() > 0 {
							failedMessages.Add(-1)
						}
					}
				} else {
					message := "The previous APPEND still could not be matched to one destination message; no additional copy was created."
					_ = s.db.FailMessageCode(ctx, id, folder.ID, uid, domain.MessageUnknown, "TB-MAIL-TARGET-APPEND-UNKNOWN", message)
					emit(folder.SourceName, uid, domain.MigrationRunning, "[TB-MAIL-TARGET-APPEND-UNKNOWN] "+message)
				}
				continue
			}
			messageID, headerErr := src.FetchMessageID(ctx, uid)
			meta.MessageID = messageID
			skip, err := s.db.BeginMessage(ctx, id, folder.ID, uidValidity, uid, meta.Size, meta.MessageID, meta.InternalDate)
			if err != nil || skip {
				continue
			}
			existing, _ = s.db.MessageTransfer(ctx, id, folder.ID, uidValidity, uid)
			if headerErr != nil {
				_ = s.db.AddMessageWarning(ctx, id, folder.ID, uid, "TB-MAIL-SOURCE-METADATA-PARTIAL", "Message-ID could not be read; the raw body is processed unchanged.")
			} else if messageID == "" {
				_ = s.db.AddMessageWarning(ctx, id, folder.ID, uid, "TB-MAIL-SOURCE-NO-MESSAGE-ID", "The message has no usable Message-ID.")
			}
			if existing.PolicyOverride == string(domain.MailIssueVerifyAgain) {
				destinationSHA, verifyErr := verifyDestinationMessage(ctx, dst, existing.DestinationUID, meta.Size, existing.SourceSHA, true)
				if verifyErr != nil {
					var typed *mailIssueError
					if errors.As(verifyErr, &typed) {
						_ = s.db.FailMessageCode(ctx, id, folder.ID, uid, typed.state, typed.code, typed.text)
					} else {
						_ = s.db.FailMessageCode(ctx, id, folder.ID, uid, domain.MessageFailed, "TB-MAIL-VERIFY-READ", verifyErr.Error())
					}
					failedMessages.Add(1)
					continue
				}
				if err := s.db.CompleteVerifiedMessage(ctx, id, folder.ID, uidValidity, uid, existing.DestinationUID, meta.Size, existing.SourceSHA, destinationSHA, "verified"); err == nil {
					copiedMessages.Add(1)
					copiedBytes.Add(meta.Size)
				}
				continue
			}
			if limits.AppendLimit > 0 && meta.SizeKnown && meta.Size > limits.AppendLimit {
				message := fmt.Sprintf("The message is %d bytes, which exceeds the APPENDLIMIT of %d bytes.", meta.Size, limits.AppendLimit)
				_ = s.db.FailMessageCode(ctx, id, folder.ID, uid, domain.MessageQuarantined, "TB-MAIL-TARGET-APPEND-LIMIT", message)
				failedMessages.Add(1)
				continue
			}
			if limits.QuotaAvailableBytes > 0 && meta.SizeKnown && meta.Size > limits.QuotaAvailableBytes {
				message := "The free storage reported by the destination is insufficient for this message."
				_ = s.db.FailMessageCode(ctx, id, folder.ID, uid, domain.MessageFailed, "TB-MAIL-TARGET-QUOTA", message)
				failedMessages.Add(1)
				emit(folder.SourceName, uid, domain.MigrationRunning, "[TB-MAIL-TARGET-QUOTA] "+message)
				return
			}
			if request.Options.DuplicateProtection {
				candidateUID, sourceSHA, sourceSize, found, duplicateErr := duplicates.findExact(ctx, src, dst, uid, meta)
				if duplicateErr != nil {
					var typed *mailIssueError
					if errors.As(duplicateErr, &typed) {
						_ = s.db.FailMessageCode(ctx, id, folder.ID, uid, typed.state, typed.code, typed.text)
					} else {
						_ = s.db.FailMessageCode(ctx, id, folder.ID, uid, domain.MessageFailed, "TB-MAIL-DUPLICATE-CHECK", security.SanitizeLogValue(duplicateErr.Error()))
					}
					failedMessages.Add(1)
					emit(folder.SourceName, uid, domain.MigrationRunning, "[TB-MAIL-DUPLICATE-CHECK] The destination message could not be checked safely for duplicates.")
					continue
				}
				if found {
					if err := s.db.CompleteVerifiedMessage(ctx, id, folder.ID, uidValidity, uid, candidateUID, sourceSize, sourceSHA, sourceSHA, "deduplicated"); err != nil {
						s.fail(ctx, id, folder.ID, uid, err, 1, failedMessages, emit, folder.SourceName, request.Source.Password, request.Destination.Password)
						continue
					}
					copiedMessages.Add(1)
					copiedBytes.Add(sourceSize)
					continue
				}
			}
			var appendResult mailimap.AppendResult
			var sha string
			var transferErr error
			transferredSize := meta.Size
			appendMeta := meta
			if !request.Options.PreserveFlags {
				appendMeta.Flags = nil
			}
			if !request.Options.PreserveDate {
				appendMeta.InternalDate = time.Time{}
			}
			for attempt := 0; attempt < request.Options.MaximumRetries; attempt++ {
				if attempt > 0 {
					message := security.SanitizeLogValue(transferErr.Error())
					_ = s.db.FailMessage(context.Background(), id, folder.ID, uid, domain.MessageRetryPending, message)
					emit(folder.SourceName, uid, domain.MigrationRunning, fmt.Sprintf("Temporary error; attempt %d/%d follows. %s", attempt+1, request.Options.MaximumRetries, message))
					timer := time.NewTimer(retry.Backoff(attempt - 1))
					select {
					case <-ctx.Done():
						timer.Stop()
						return
					case <-timer.C:
					}
					_ = src.Close()
					_ = dst.Close()
					connectionUsable = false
					var nextSource, nextDestination mailimap.Client
					nextSource, nextDestination, transferErr = connect()
					if transferErr != nil {
						if !retry.IsTransient(transferErr) {
							break
						}
						continue
					}
					src, dst = nextSource, nextDestination
					connectionUsable = true
					if _, _, _, transferErr = src.SelectMailbox(ctx, folder.SourceName, true); transferErr != nil {
						continue
					}
					if destinationUIDValidity, _, allowed, transferErr = dst.SelectMailbox(ctx, folder.DestinationName, false); transferErr != nil {
						continue
					}
					_, _ = s.db.BeginMessage(ctx, id, folder.ID, uidValidity, uid, meta.Size, meta.MessageID, meta.InternalDate)
				}

				_, preAppendUIDNext, refreshedAllowed, selectErr := dst.SelectMailbox(ctx, folder.DestinationName, false)
				if selectErr != nil {
					transferErr = selectErr
					continue
				}
				allowed = refreshedAllowed
				if stateErr := s.db.SetMessageTransferIdentity(ctx, id, folder.ID, uid, "", preAppendUIDNext); stateErr != nil {
					transferErr = issue(domain.MessageFailed, "TB-MAIL-STATE-PERSIST", "The safe pre-APPEND state could not be stored locally; APPEND was not started.")
					break
				}
				actualSize := meta.Size
				transferErr = src.StreamMessage(ctx, uid, func(reader io.Reader, size int64) error {
					actualSize = size
					transferredSize = size
					if size == 0 && !forceEmpty {
						return issue(domain.MessageQuarantined, "TB-MAIL-SOURCE-EMPTY", "The source raw body is empty.")
					}
					if meta.SizeKnown && size != meta.Size {
						return issue(domain.MessageQuarantined, "TB-MAIL-SOURCE-SIZE-MISMATCH", fmt.Sprintf("Literal size %d does not match RFC822.SIZE %d.", size, meta.Size))
					}
					if limits.AppendLimit > 0 && size > limits.AppendLimit {
						return issue(domain.MessageQuarantined, "TB-MAIL-TARGET-APPEND-LIMIT", fmt.Sprintf("The message is %d bytes, which exceeds the APPENDLIMIT of %d bytes.", size, limits.AppendLimit))
					}
					appendMeta.Size = size
					hash := sha256.New()
					result, appendErr := dst.AppendMessage(ctx, folder.DestinationName, appendMeta, io.TeeReader(reader, hash), allowed, request.Options.ExcludedKeywords)
					if appendErr == nil || mailimap.IsUncertainAppend(appendErr) {
						sha = hex.EncodeToString(hash.Sum(nil))
						appendResult = result
					}
					return appendErr
				})
				_ = s.db.SetMessageTransferIdentity(ctx, id, folder.ID, uid, sha, preAppendUIDNext)
				if mailimap.IsUncertainAppend(transferErr) {
					uncertainErr := transferErr
					_ = src.Close()
					_ = dst.Close()
					connectionUsable = false
					var nextSource, nextDestination mailimap.Client
					nextSource, nextDestination, err = connect()
					if err != nil {
						transferErr = issue(domain.MessageUnknown, "TB-MAIL-TARGET-APPEND-UNKNOWN", "The APPEND outcome is unknown and the destination could not be checked again.")
						break
					}
					src, dst = nextSource, nextDestination
					connectionUsable = true
					if _, _, _, err = src.SelectMailbox(ctx, folder.SourceName, true); err != nil {
						transferErr = issue(domain.MessageUnknown, "TB-MAIL-TARGET-APPEND-UNKNOWN", "The APPEND outcome is unknown; the source could not be opened again.")
						break
					}
					if destinationUIDValidity, _, allowed, err = dst.SelectMailbox(ctx, folder.DestinationName, false); err != nil {
						transferErr = issue(domain.MessageUnknown, "TB-MAIL-TARGET-APPEND-UNKNOWN", "The APPEND outcome is unknown; the destination could not be inventoried.")
						break
					}
					matches, matchErr := matchingDestinationUIDs(ctx, dst, uidBefore(preAppendUIDNext), actualSize, sha)
					if matchErr != nil || len(matches) > 1 {
						transferErr = issue(domain.MessageUnknown, "TB-MAIL-TARGET-APPEND-UNKNOWN", "The APPEND outcome could not be resolved unambiguously.")
						break
					}
					if len(matches) == 1 {
						appendResult.UID = matches[0]
						transferErr = nil
					} else if isQuotaError(uncertainErr) {
						transferErr = issue(domain.MessageFailed, "TB-MAIL-TARGET-QUOTA", "The destination rejected APPEND because its quota is exhausted.")
					} else if isAppendLimitError(uncertainErr) {
						transferErr = issue(domain.MessageQuarantined, "TB-MAIL-TARGET-APPEND-LIMIT", "The destination rejected the message because of its size limit.")
					} else {
						transferErr = fmt.Errorf("temporary APPEND result absent after destination inventory")
					}
				}
				if transferErr == nil && appendResult.UID == 0 {
					matches, matchErr := matchingDestinationUIDs(ctx, dst, uidBefore(preAppendUIDNext), actualSize, sha)
					if matchErr != nil || len(matches) != 1 {
						transferErr = issue(domain.MessageUnknown, "TB-MAIL-TARGET-APPEND-UNKNOWN", "The server returned no destination UID and the new message could not be identified unambiguously.")
					} else {
						appendResult.UID = matches[0]
					}
				}
				if transferErr == nil {
					_ = s.db.MarkMessageVerifying(ctx, id, folder.ID, uid, appendResult.UID, sha)
					fullHash := request.Options.VerificationMode != domain.VerificationMetadata
					destinationSHA, verifyErr := verifyDestinationMessage(ctx, dst, appendResult.UID, actualSize, sha, fullHash)
					if verifyErr != nil {
						transferErr = verifyErr
					} else {
						verification := "metadata"
						if fullHash {
							verification = "verified"
						}
						if completeErr := s.db.CompleteVerifiedMessage(ctx, id, folder.ID, uidValidity, uid, appendResult.UID, actualSize, sha, destinationSHA, verification); completeErr != nil {
							transferErr = issue(domain.MessageUnknown, "TB-MAIL-STATE-UNKNOWN", "The destination message was verified, but the final local state could not be stored.")
						}
					}
				}
				if transferErr == nil || !retry.IsTransient(transferErr) {
					break
				}
			}
			if transferErr != nil {
				var typed *mailIssueError
				if errors.As(transferErr, &typed) {
					_ = s.db.FailMessageCode(context.Background(), id, folder.ID, uid, typed.state, typed.code, typed.text)
					failedMessages.Add(1)
					emit(folder.SourceName, uid, domain.MigrationRunning, typed.Error())
				} else {
					s.fail(ctx, id, folder.ID, uid, transferErr, 1, failedMessages, emit, folder.SourceName, request.Source.Password, request.Destination.Password)
				}
				if isQuotaError(transferErr) {
					return
				}
				if !connectionUsable {
					return
				}
				continue
			}
			copiedMessages.Add(1)
			copiedBytes.Add(transferredSize)
			duplicates.addConsumed(appendResult.UID, transferredSize, sha)
			if limits.QuotaAvailableBytes > transferredSize {
				limits.QuotaAvailableBytes -= transferredSize
			}
		}
	}
}

// reconcileFolder validates the previously tracked destination identity for
// every source message that still exists. A missing destination copy is moved
// back to PENDING, which makes UnfinishedUIDs feed it into the normal,
// resumable transfer path below.
func (s *Service) reconcileFolder(ctx context.Context, migrationID int64, request domain.StartRequest, folder database.FolderRecord, sourceUIDValidity, destinationUIDValidity uint32, sourceUIDs []uint32, dst mailimap.Client, copiedBytes, copiedMessages, runItemsDone *atomic.Int64) error {
	records, err := s.db.CopiedMessages(ctx, migrationID, folder.ID, sourceUIDValidity)
	if err != nil || len(records) == 0 {
		return err
	}
	destinationUIDs, err := dst.SearchUIDs(ctx, 0)
	if err != nil {
		return fmt.Errorf("check destination folder %q: %w", folder.DestinationName, err)
	}
	sourceSet := uidSet(sourceUIDs)
	destinationSet := uidSet(destinationUIDs)
	stableDestinationUIDs := folder.DestinationUIDValidity != 0 && folder.DestinationUIDValidity == destinationUIDValidity

	for _, record := range records {
		runItemsDone.Add(1)
		_, stillInSource := sourceSet[record.SourceUID]
		present := false
		confirmedUID := record.DestinationUID
		if stableDestinationUIDs && record.DestinationUID != 0 {
			if _, exists := destinationSet[record.DestinationUID]; exists {
				present = true
			}
		}
		if !stableDestinationUIDs {
			// UIDs have no identity across UIDVALIDITY changes. Only a unique
			// byte-for-byte match may rebind a previously copied destination.
			if record.SourceSHA == "" {
				continue
			}
			matches, matchErr := matchingDestinationUIDs(ctx, dst, 0, record.Size, record.SourceSHA)
			if matchErr != nil {
				return fmt.Errorf("check message %d after UIDVALIDITY change: %w", record.SourceUID, matchErr)
			}
			if len(matches) != 1 {
				continue
			}
			present = true
			confirmedUID = matches[0]
		} else if !present && record.MessageID != "" {
			candidate, findErr := dst.FindCandidate(ctx, folder.DestinationName, mailimap.MessageMetadata{MessageID: record.MessageID, Size: record.Size, InternalDate: record.InternalDate})
			if findErr != nil {
				return fmt.Errorf("reconcile message %d at the destination: %w", record.SourceUID, findErr)
			}
			if candidate.Found {
				present = true
				confirmedUID = candidate.UID
			}
		}
		if present {
			if confirmedUID != record.DestinationUID {
				if err := s.db.ConfirmDestination(ctx, record.ID, confirmedUID); err != nil {
					return err
				}
			}
			if !stillInSource {
				summary := mailimap.MessageSummary{}
				if summaryClient, ok := dst.(mailimap.SummaryClient); ok {
					summary, _ = summaryClient.FetchSummary(ctx, confirmedUID)
				}
				if err := s.db.UpsertSourceDeletion(ctx, migrationID, record.ID, confirmedUID, destinationUIDValidity, summary.Subject, summary.From); err != nil {
					return err
				}
				continue
			}
			_ = s.db.ClearSourceDeletion(ctx, record.ID)
			continue
		}
		if !stillInSource {
			// Both copies are gone, so there is no target-side decision to make.
			// Completed decisions remain as an audit trail for reports.
			_ = s.db.ClearUnresolvedSourceDeletion(ctx, record.ID)
			continue
		}
		requeued, size, requeueErr := s.db.RequeueCopiedMessage(ctx, migrationID, record.ID)
		if requeueErr != nil {
			return requeueErr
		}
		if requeued {
			copiedMessages.Add(-1)
			copiedBytes.Add(-size)
		}
	}
	return nil
}

func storedMetadataMatches(record database.CopiedMessageRecord, metadata mailimap.MessageMetadata, compareDate bool) bool {
	if record.Size != metadata.Size {
		return false
	}
	if record.MessageID != "" && record.MessageID != metadata.MessageID {
		return false
	}
	if compareDate && !record.InternalDate.IsZero() && !metadata.InternalDate.IsZero() {
		difference := record.InternalDate.Sub(metadata.InternalDate)
		if difference < 0 {
			difference = -difference
		}
		if difference > time.Second {
			return false
		}
	}
	return true
}

func uidSet(uids []uint32) map[uint32]struct{} {
	set := make(map[uint32]struct{}, len(uids))
	for _, uid := range uids {
		set[uid] = struct{}{}
	}
	return set
}

// ResolveSourceDeletions applies explicit target-only decisions after a
// completed reconciliation. It never exposes mutation methods to the source
// client and never falls back to a mailbox-wide EXPUNGE.
func (s *Service) ResolveSourceDeletions(ctx context.Context, migrationID int64, destination domain.AccountConfig, options domain.TransferOptions, actions []domain.SourceDeletionAction) error {
	if migrationID <= 0 || len(actions) == 0 {
		return errors.New("no source deletions selected")
	}
	seen := make(map[int64]struct{}, len(actions))
	var destructive []domain.SourceDeletionAction
	for _, action := range actions {
		if _, duplicate := seen[action.ID]; duplicate {
			return errors.New("a source deletion was selected more than once")
		}
		seen[action.ID] = struct{}{}
		switch action.Resolution {
		case domain.SourceDeletionKeep:
		case domain.SourceDeletionTrash, domain.SourceDeletionDelete:
			destructive = append(destructive, action)
		default:
			return errors.New("invalid source deletion action")
		}
	}
	for _, action := range actions {
		if action.Resolution == domain.SourceDeletionKeep {
			if err := s.db.ResolveSourceDeletionKeep(ctx, migrationID, action.ID); err != nil {
				return err
			}
		}
	}
	if len(destructive) == 0 {
		return nil
	}
	factory, ok := s.factory.(mailimap.DestinationFactory)
	if !ok {
		return errors.New("the IMAP adapter does not support safe destination changes")
	}
	timeout := time.Duration(options.ConnectionTimeout) * time.Second
	stall := time.Duration(options.StallTimeout) * time.Second
	client, err := factory.ConnectDestination(ctx, destination, timeout, stall)
	if err != nil {
		return err
	}
	defer client.Close()
	mailboxes, err := client.ListMailboxes(ctx)
	if err != nil {
		return fmt.Errorf("check destination folders before deletion: %w", err)
	}
	trashFolder := ""
	for _, mailbox := range mailboxes {
		if mailbox.Selectable && strings.EqualFold(mailbox.SpecialUse, "\\Trash") {
			trashFolder = mailbox.Name
			break
		}
	}
	for _, action := range destructive {
		record, recordErr := s.db.SourceDeletionRecord(ctx, migrationID, action.ID)
		if recordErr != nil {
			return recordErr
		}
		if record.Status != "pending" && record.Status != "failed" {
			return errors.New("source deletion has already been processed")
		}
		failure := func(cause error) {
			message := security.RedactError(cause.Error(), destination.Password)
			message = security.SanitizeLogValue(message)
			_ = s.db.FailSourceDeletion(context.Background(), migrationID, action.ID, action.Resolution, message)
		}
		uidValidity, _, _, selectErr := client.SelectMailbox(ctx, record.DestinationFolder, false)
		if selectErr != nil {
			failure(fmt.Errorf("open destination folder %q: %w", record.DestinationFolder, selectErr))
			continue
		}
		uid := record.DestinationUID
		if uidValidity != record.DestinationUIDValidity {
			if record.SourceSHA == "" {
				failure(errors.New("UIDVALIDITY changed and a hash is missing for safe remapping"))
				continue
			}
			matches, matchErr := matchingDestinationUIDs(ctx, client, 0, record.Size, record.SourceSHA)
			if matchErr != nil || len(matches) != 1 {
				if matchErr == nil {
					matchErr = fmt.Errorf("hash matched %d destination candidates", len(matches))
				}
				failure(fmt.Errorf("destination message is ambiguous after UIDVALIDITY change: %w", matchErr))
				continue
			}
			uid = matches[0]
		}
		if record.SourceSHA != "" {
			if _, hashErr := verifyDestinationMessage(ctx, client, uid, record.Size, record.SourceSHA, true); hashErr != nil {
				failure(fmt.Errorf("check destination hash: %w", hashErr))
				continue
			}
		} else {
			// Older metadata-only records have no content hash and therefore need
			// the strict header identity check. When a verified source hash exists,
			// the byte-for-byte check above is authoritative; requiring a partial
			// Message-ID fetch as well rejects valid mails on some IMAP servers.
			metadata, metaErr := client.FetchMetadata(ctx, uid)
			if metaErr != nil || !metadata.SizeKnown || metadata.Size != record.Size || (record.MessageHeaderID != "" && metadata.MessageID != record.MessageHeaderID) {
				if metaErr == nil {
					metaErr = errors.New("stored metadata does not match the destination message")
				}
				failure(fmt.Errorf("check destination identity: %w", metaErr))
				continue
			}
		}
		// Hash/candidate checks may have switched to a read-only selection.
		if _, _, _, selectErr = client.SelectMailbox(ctx, record.DestinationFolder, false); selectErr != nil {
			failure(fmt.Errorf("reopen destination folder: %w", selectErr))
			continue
		}
		var mutationErr error
		if action.Resolution == domain.SourceDeletionTrash {
			if trashFolder == "" {
				mutationErr = errors.New("the destination server does not report a safe trash folder")
			} else if strings.EqualFold(mailboxKey(trashFolder), mailboxKey(record.DestinationFolder)) {
				mutationErr = errors.New("the message is already in trash")
			} else {
				mutationErr = client.MoveMessage(ctx, uid, trashFolder)
			}
		} else {
			mutationErr = client.DeleteMessage(ctx, uid)
		}
		if mutationErr != nil {
			failure(mutationErr)
			continue
		}
		if err := s.db.CompleteSourceDeletion(ctx, migrationID, action.ID, action.Resolution); err != nil {
			return err
		}
	}
	return nil
}

func uidsAfter(uids []uint32, after uint32) []uint32 {
	result := make([]uint32, 0, len(uids))
	for _, uid := range uids {
		if uid > after {
			result = append(result, uid)
		}
	}
	return result
}

func (s *Service) fail(ctx context.Context, id, folderID int64, uid uint32, err error, maxRetries int, failed *atomic.Int64, emit emitFunc, folder string, secrets ...string) {
	state := domain.MessageRetryPending
	if !retry.IsTransient(err) || maxRetries <= 1 {
		state = domain.MessageFailed
		failed.Add(1)
	}
	message := security.RedactError(err.Error(), secrets...)
	_ = s.db.FailMessage(context.Background(), id, folderID, uid, state, message)
	emit(folder, uid, domain.MigrationRunning, message)
}
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
func containsMailbox(mailboxes []domain.Mailbox, needle string) bool {
	for _, m := range mailboxes {
		if strings.Contains(strings.ToLower(m.Name), strings.ToLower(needle)) {
			return true
		}
	}
	return false
}

func mergeUIDs(left, right []uint32) []uint32 {
	seen := make(map[uint32]struct{}, len(left)+len(right))
	result := make([]uint32, 0, len(left)+len(right))
	for _, set := range [][]uint32{left, right} {
		for _, uid := range set {
			if uid == 0 {
				continue
			}
			if _, ok := seen[uid]; ok {
				continue
			}
			seen[uid] = struct{}{}
			result = append(result, uid)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result
}
