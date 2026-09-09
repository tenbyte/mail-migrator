package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	goruntime "runtime"
	"runtime/debug"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/wailsapp/wails/v2/pkg/runtime"

	"github.com/tenbyte/mail-migrator/internal/credentials"
	"github.com/tenbyte/mail-migrator/internal/database"
	"github.com/tenbyte/mail-migrator/internal/dav"
	"github.com/tenbyte/mail-migrator/internal/davmigration"
	"github.com/tenbyte/mail-migrator/internal/diagnostics"
	"github.com/tenbyte/mail-migrator/internal/domain"
	"github.com/tenbyte/mail-migrator/internal/folders"
	"github.com/tenbyte/mail-migrator/internal/mailimap"
	"github.com/tenbyte/mail-migrator/internal/migration"
	"github.com/tenbyte/mail-migrator/internal/updatecheck"
)

type App struct {
	ctx                  context.Context
	lifecycleMu          sync.Mutex
	db                   *database.DB
	databasePath         string
	migrations           *migration.Service
	davMigrations        *davmigration.Service
	credentials          credentialStore
	startupErr           error
	deletionMu           sync.Mutex
	deletionDestinations map[int64]domain.AccountConfig
	progressMu           sync.Mutex
	runProgress          map[int64]map[domain.ServiceKind]domain.Progress
	updateChecker        updateChecker
	updateOnce           sync.Once
	updateInfo           domain.UpdateInfo
	reloadApplication    func(context.Context)
	resetBlocked         func() bool
	removeStateFiles     func(string) error
	diagnostics          *diagnostics.Manager
	copyToClipboard      func(context.Context, string) error
	openDiagnostics      func(string) error
}

type credentialStore interface {
	Set(id, password string) error
	Get(id string) (string, error)
	DeleteAll() error
}

type updateChecker interface {
	Check(context.Context, string) (domain.UpdateInfo, error)
}

func NewApp(managers ...*diagnostics.Manager) *App {
	var manager *diagnostics.Manager
	if len(managers) > 0 {
		manager = managers[0]
	}
	return &App{
		credentials: credentials.Store{}, deletionDestinations: make(map[int64]domain.AccountConfig), runProgress: make(map[int64]map[domain.ServiceKind]domain.Progress),
		updateChecker: updatecheck.New(), reloadApplication: runtime.WindowReloadApp, removeStateFiles: database.RemoveStateFiles,
		diagnostics: manager, copyToClipboard: runtime.ClipboardSetText, openDiagnostics: openDirectory,
	}
}

func (a *App) domReady(context.Context) { hideZoomButton() }

func (a *App) startup(ctx context.Context) {
	a.lifecycleMu.Lock()
	defer a.lifecycleMu.Unlock()
	a.ctx = ctx
	if a.diagnostics != nil {
		a.diagnostics.InfoEvent("application_startup", diagnostics.Fields{})
	}
	path, err := database.DefaultPath()
	if err != nil {
		a.startupErr = err
		if a.diagnostics != nil {
			a.diagnostics.ErrorEvent("database_path_failed", diagnostics.Fields{ErrorCode: "TB-DIAG-DB-PATH", Message: err.Error()})
		}
		return
	}
	a.databasePath = path
	if err := a.openDatabase(ctx, path); err != nil {
		a.startupErr = err
		if a.diagnostics != nil {
			a.diagnostics.ErrorEvent("database_open_failed", diagnostics.Fields{ErrorCode: "TB-DIAG-DB-OPEN", Message: err.Error()})
		}
		return
	}
	if a.diagnostics != nil {
		a.diagnostics.InfoEvent("application_ready", diagnostics.Fields{})
	}
}

func (a *App) openDatabase(ctx context.Context, path string) error {
	db, err := database.Open(path)
	if err != nil {
		return err
	}
	if err := db.RecoverInterrupted(ctx); err != nil {
		_ = db.Close()
		return err
	}
	a.db = db
	a.migrations = migration.New(db, mailimap.RealFactory{}, a.emitProgress, a.diagnostics)
	a.davMigrations = davmigration.New(db, dav.RealFactory{}, a.emitProgress, a.diagnostics)
	a.startupErr = nil
	return nil
}

func (a *App) emitProgress(progress domain.Progress) {
	a.progressMu.Lock()
	previous := a.runProgress[progress.MigrationID][progress.Service]
	if progress.Service != "" {
		if a.runProgress[progress.MigrationID] == nil {
			a.runProgress[progress.MigrationID] = make(map[domain.ServiceKind]domain.Progress)
		}
		a.runProgress[progress.MigrationID][progress.Service] = progress
	}
	runSnapshot := make(map[domain.ServiceKind]domain.Progress, len(a.runProgress[progress.MigrationID]))
	for kind, item := range a.runProgress[progress.MigrationID] {
		runSnapshot[kind] = item
	}
	a.progressMu.Unlock()
	if a.diagnostics != nil && (previous.State != progress.State || previous.RunPhase != progress.RunPhase) {
		a.diagnostics.InfoEvent("migration_progress", diagnostics.Fields{
			MigrationID: progress.MigrationID, Service: string(progress.Service), Phase: progress.RunPhase, State: string(progress.State),
			Values: map[string]any{"runItemsTotal": progress.RunItemsTotal, "runItemsDone": progress.RunItemsDone, "messagesFailed": progress.MessagesFailed},
		})
	}
	if a.db != nil {
		progress.Services, _ = a.db.ServiceProgresses(context.Background(), progress.MigrationID)
		for index := range progress.Services {
			if current, ok := runSnapshot[progress.Services[index].Kind]; ok {
				progress.Services[index].RunMode = current.RunMode
				progress.Services[index].RunPhase = current.RunPhase
				progress.Services[index].RunItemsTotal = current.RunItemsTotal
				progress.Services[index].RunItemsDone = current.RunItemsDone
				progress.Services[index].RunIndeterminate = current.RunIndeterminate
			}
		}
		if aggregate, err := a.db.RecentByID(context.Background(), progress.MigrationID); err == nil {
			progress.State = aggregate.State
			progress.MessagesTotal = aggregate.MessagesTotal
			progress.MessagesCopied = aggregate.MessagesCopied
			progress.MessagesFailed = aggregate.MessagesFailed
			progress.BytesTotal = aggregate.BytesTotal
			progress.BytesCopied = aggregate.BytesCopied
		}
	}
	progress.RunItemsTotal = 0
	progress.RunItemsDone = 0
	progress.RunIndeterminate = false
	for _, item := range runSnapshot {
		progress.RunItemsTotal += item.RunItemsTotal
		progress.RunItemsDone += item.RunItemsDone
		progress.RunIndeterminate = progress.RunIndeterminate || item.RunIndeterminate
		if item.RunMode == "reconcile" {
			progress.RunMode = "reconcile"
		}
	}
	runtime.EventsEmit(a.ctx, "migration:progress", progress)
	runtime.EventsEmit(a.ctx, "job:progress", progress)
	if isTerminalMigrationState(progress.State) {
		a.progressMu.Lock()
		delete(a.runProgress, progress.MigrationID)
		a.progressMu.Unlock()
	}
}

func isTerminalMigrationState(state domain.MigrationState) bool {
	switch state {
	case domain.MigrationCompleted, domain.MigrationCompletedWithErrors, domain.MigrationFailed, domain.MigrationCancelled:
		return true
	default:
		return false
	}
}

func (a *App) shutdown(context.Context) {
	a.lifecycleMu.Lock()
	defer a.lifecycleMu.Unlock()
	a.deletionMu.Lock()
	clear(a.deletionDestinations)
	a.deletionMu.Unlock()
	a.progressMu.Lock()
	clear(a.runProgress)
	a.progressMu.Unlock()
	if a.db != nil {
		_ = a.db.Close()
		a.db = nil
	}
	a.migrations = nil
	a.davMigrations = nil
	if a.diagnostics != nil {
		a.diagnostics.InfoEvent("application_shutdown", diagnostics.Fields{})
	}
}

func (a *App) ensureReady() error {
	if a.startupErr != nil {
		return fmt.Errorf("application database unavailable: %w", a.startupErr)
	}
	if a.migrations == nil || a.davMigrations == nil {
		return errors.New("application is still starting")
	}
	return nil
}

func (a *App) resetIsBlocked() bool {
	if a.resetBlocked != nil && a.resetBlocked() {
		return true
	}
	return a.migrations != nil && a.migrations.Active() || a.davMigrations != nil && a.davMigrations.Active()
}

func (a *App) clearTransientMigrationState() {
	a.deletionMu.Lock()
	clear(a.deletionDestinations)
	a.deletionMu.Unlock()
	a.progressMu.Lock()
	clear(a.runProgress)
	a.progressMu.Unlock()
}

func (a *App) resetMigrationData(preserveSettings bool) error {
	a.lifecycleMu.Lock()
	defer a.lifecycleMu.Unlock()
	if a.resetIsBlocked() {
		return errors.New("a migration is currently active; finish or cancel it before resetting local data")
	}
	path := a.databasePath
	if path == "" {
		var err error
		path, err = database.DefaultPath()
		if err != nil {
			return fmt.Errorf("locate local migration data: %w", err)
		}
		a.databasePath = path
	}
	var noticeSettings domain.MailboxNoticeSettings
	if preserveSettings && a.db != nil {
		var err error
		noticeSettings, err = a.db.MailboxNoticeSettings(context.Background())
		if err != nil {
			return fmt.Errorf("read mailbox notice settings before reset: %w", err)
		}
	}
	if a.db != nil {
		if err := a.db.Close(); err != nil {
			return fmt.Errorf("close local migration database: %w", err)
		}
	}
	a.db = nil
	a.migrations = nil
	a.davMigrations = nil
	removeStateFiles := a.removeStateFiles
	if removeStateFiles == nil {
		removeStateFiles = database.RemoveStateFiles
	}
	if err := removeStateFiles(path); err != nil {
		recoveryErr := a.openDatabase(context.Background(), path)
		if recoveryErr != nil {
			a.startupErr = recoveryErr
			return fmt.Errorf("reset local migration data: %v; reopen database: %w", err, recoveryErr)
		}
		return fmt.Errorf("reset local migration data: %w; the database was reopened", err)
	}
	if err := a.openDatabase(context.Background(), path); err != nil {
		a.startupErr = err
		return fmt.Errorf("create a fresh migration database: %w", err)
	}
	if preserveSettings {
		if err := a.db.SaveMailboxNoticeSettings(context.Background(), noticeSettings); err != nil {
			return fmt.Errorf("restore mailbox notice settings: %w", err)
		}
	}
	a.clearTransientMigrationState()
	return nil
}

// ResetMigrationData removes only local migration and recovery state. Saved
// credentials and exported files are deliberately left untouched.
func (a *App) ResetMigrationData() error { return a.resetMigrationData(true) }

// FactoryReset removes local migration state and every credential stored for
// this application, then reloads the Wails application on success.
func (a *App) FactoryReset() error {
	if err := a.resetMigrationData(false); err != nil {
		return err
	}
	if err := a.credentials.DeleteAll(); err != nil {
		return fmt.Errorf("migration data was reset, but stored passwords could not be removed: %w", err)
	}
	if a.diagnostics != nil {
		if err := a.diagnostics.Clear(); err != nil {
			return fmt.Errorf("migration data and stored passwords were reset, but diagnostic logs could not be removed: %w", err)
		}
	}
	if a.reloadApplication != nil {
		a.reloadApplication(a.ctx)
	}
	return nil
}

func (a *App) Defaults() domain.TransferOptions { return domain.DefaultTransferOptions() }

func (a *App) DiagnosticsInfo() domain.DiagnosticsInfo {
	if a.diagnostics == nil {
		return domain.DiagnosticsInfo{}
	}
	return domain.DiagnosticsInfo{LogDirectory: a.diagnostics.Directory(), ActiveLog: a.diagnostics.LogPath(), SessionID: a.diagnostics.SessionID()}
}

func (a *App) ReportFrontendError(report domain.FrontendErrorReport) error {
	if strings.TrimSpace(report.Message) == "" {
		return errors.New("frontend error message is required")
	}
	if len(report.Message)+len(report.Stack)+len(report.ComponentStack)+len(report.View) > 128<<10 {
		return errors.New("frontend error report is too large")
	}
	if a.diagnostics == nil {
		return errors.New("diagnostics are unavailable")
	}
	_, err := a.diagnostics.RecordFrontendError(diagnostics.FrontendError{
		Message: report.Message, Stack: report.Stack, ComponentStack: report.ComponentStack, View: report.View, Fatal: report.Fatal,
	})
	return err
}

func (a *App) CopyDiagnostics() error {
	if a.diagnostics == nil {
		return errors.New("diagnostics are unavailable")
	}
	text, err := a.diagnostics.Snapshot(200)
	if err != nil {
		return err
	}
	copyText := a.copyToClipboard
	if copyText == nil {
		copyText = runtime.ClipboardSetText
	}
	if err := copyText(a.ctx, text); err != nil {
		return fmt.Errorf("copy diagnostics to clipboard: %w", err)
	}
	return nil
}

func (a *App) OpenDiagnosticsDirectory() error {
	if a.diagnostics == nil {
		return errors.New("diagnostics are unavailable")
	}
	opener := a.openDiagnostics
	if opener == nil {
		opener = openDirectory
	}
	return opener(a.diagnostics.Directory())
}

func (a *App) MailboxNoticeSettings() (domain.MailboxNoticeSettings, error) {
	if err := a.ensureReady(); err != nil {
		return domain.MailboxNoticeSettings{}, err
	}
	return a.db.MailboxNoticeSettings(a.ctx)
}

func (a *App) SaveMailboxNoticeSettings(settings domain.MailboxNoticeSettings) error {
	if err := a.ensureReady(); err != nil {
		return err
	}
	if err := validateMailboxNoticeSettings(&settings); err != nil {
		return err
	}
	return a.db.SaveMailboxNoticeSettings(a.ctx, settings)
}

func normalizeNoticeText(value string) string {
	value = strings.ReplaceAll(value, "\r\n", "\n")
	return strings.ReplaceAll(value, "\r", "\n")
}

func validateMailboxNoticeSettings(settings *domain.MailboxNoticeSettings) error {
	settings.Source.CustomText = normalizeNoticeText(settings.Source.CustomText)
	settings.Destination.CustomText = normalizeNoticeText(settings.Destination.CustomText)
	if utf8.RuneCountInString(settings.Source.CustomText) > 10000 || utf8.RuneCountInString(settings.Destination.CustomText) > 10000 {
		return errors.New("mailbox notice custom text cannot exceed 10,000 characters")
	}
	return nil
}

func (a *App) CheckForUpdate() domain.UpdateInfo {
	a.updateOnce.Do(func() {
		checker := a.updateChecker
		if checker == nil {
			checker = updatecheck.New()
		}
		ctx := a.ctx
		if ctx == nil {
			ctx = context.Background()
		}
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		a.updateInfo, _ = checker.Check(ctx, appVersion)
	})
	return a.updateInfo
}

func (a *App) OpenLatestRelease() {
	runtime.BrowserOpenURL(a.ctx, updatecheck.LatestReleaseURL)
}

func (a *App) protectMailDiagnostics(accounts ...domain.AccountConfig) {
	if a.diagnostics == nil {
		return
	}
	for _, account := range accounts {
		a.diagnostics.Protect(account.Host, account.Username, account.Password, account.CredentialID)
	}
}

func (a *App) protectDAVDiagnostics(endpoints ...domain.DAVEndpoint) {
	if a.diagnostics == nil {
		return
	}
	for _, endpoint := range endpoints {
		a.diagnostics.Protect(endpoint.URL, endpoint.Username, endpoint.Password, endpoint.CredentialID)
	}
}

func (a *App) TestDAVAccount(kind domain.ServiceKind, endpoint domain.DAVEndpoint) (domain.DAVAccountSummary, error) {
	if err := a.ensureReady(); err != nil {
		return domain.DAVAccountSummary{}, err
	}
	a.protectDAVDiagnostics(endpoint)
	ctx, cancel := context.WithTimeout(a.ctx, 5*time.Minute)
	defer cancel()
	return a.davMigrations.Inspect(ctx, kind, endpoint)
}

func (a *App) AnalyseJob(request domain.JobPreflightRequest) (domain.JobPreflightResult, error) {
	if err := a.ensureReady(); err != nil {
		return domain.JobPreflightResult{}, err
	}
	if !request.MailEnabled && !request.Calendar.Enabled && !request.Contacts.Enabled {
		return domain.JobPreflightResult{}, errors.New("select at least mail, calendar, or contacts")
	}
	a.protectMailDiagnostics(request.MailSource, request.MailDestination)
	a.protectDAVDiagnostics(request.Calendar.Source, request.Calendar.Destination, request.Contacts.Source, request.Contacts.Destination)
	if a.diagnostics != nil {
		a.diagnostics.InfoEvent("preflight_started", diagnostics.Fields{Values: map[string]any{"mail": request.MailEnabled, "calendar": request.Calendar.Enabled, "contacts": request.Contacts.Enabled}})
	}
	ctx, cancel := context.WithTimeout(a.ctx, 30*time.Minute)
	defer cancel()
	type result struct {
		kind domain.ServiceKind
		mail domain.PreflightResult
		dav  domain.DAVPreflightResult
		err  error
	}
	count := 0
	results := make(chan result, 3)
	if request.MailEnabled {
		count++
		go func() {
			defer func() {
				if recovered := recover(); recovered != nil {
					if a.diagnostics != nil {
						a.diagnostics.RecordPanic("job_mail_preflight", 0, string(domain.ServiceMail), recovered, debug.Stack())
					}
					results <- result{kind: domain.ServiceMail, err: errors.New("internal error during mail preflight")}
				}
			}()
			value, err := a.migrations.Preflight(ctx, request.MailSource, request.MailDestination)
			results <- result{kind: domain.ServiceMail, mail: value, err: err}
		}()
	}
	for _, service := range []domain.DAVServiceRequest{request.Calendar, request.Contacts} {
		if !service.Enabled {
			continue
		}
		count++
		service := service
		go func() {
			defer func() {
				if recovered := recover(); recovered != nil {
					if a.diagnostics != nil {
						a.diagnostics.RecordPanic("job_dav_preflight", 0, string(service.Kind), recovered, debug.Stack())
					}
					results <- result{kind: service.Kind, err: errors.New("internal error during DAV preflight")}
				}
			}()
			value, err := a.davMigrations.Preflight(ctx, service.Kind, service.Source, service.Destination)
			results <- result{kind: service.Kind, dav: value, err: err}
		}()
	}
	output := domain.JobPreflightResult{Warnings: []string{}}
	for range count {
		item := <-results
		if item.err != nil {
			if a.diagnostics != nil {
				a.diagnostics.ErrorEvent("preflight_failed", diagnostics.Fields{Service: string(item.kind), ErrorCode: "TB-DIAG-PREFLIGHT", Message: item.err.Error()})
			}
			return output, item.err
		}
		switch item.kind {
		case domain.ServiceMail:
			output.Mail = &item.mail
			output.Warnings = append(output.Warnings, item.mail.Warnings...)
		case domain.ServiceCalendar:
			output.Calendar = &item.dav
			output.Warnings = append(output.Warnings, item.dav.Warnings...)
		case domain.ServiceContacts:
			output.Contacts = &item.dav
			output.Warnings = append(output.Warnings, item.dav.Warnings...)
		}
	}
	if a.diagnostics != nil {
		a.diagnostics.InfoEvent("preflight_completed", diagnostics.Fields{Values: map[string]any{"serviceCount": count}})
	}
	return output, nil
}

func (a *App) TestAccount(account domain.AccountConfig) (domain.ServerSummary, error) {
	if err := a.ensureReady(); err != nil {
		return domain.ServerSummary{}, err
	}
	a.protectMailDiagnostics(account)
	ctx, cancel := context.WithTimeout(a.ctx, 3*time.Minute)
	defer cancel()
	return a.migrations.Inspect(ctx, account)
}

func (a *App) AnalyseMailboxes(source, destination domain.AccountConfig) (domain.PreflightResult, error) {
	if err := a.ensureReady(); err != nil {
		return domain.PreflightResult{}, err
	}
	a.protectMailDiagnostics(source, destination)
	ctx, cancel := context.WithTimeout(a.ctx, 15*time.Minute)
	defer cancel()
	return a.migrations.Preflight(ctx, source, destination)
}

func (a *App) StartMigration(request domain.StartRequest) (int64, error) {
	if err := a.ensureReady(); err != nil {
		return 0, err
	}
	a.protectMailDiagnostics(request.Source, request.Destination)
	if a.diagnostics != nil {
		for _, mapping := range request.Mappings {
			a.diagnostics.Protect(mapping.Source.Name, mapping.DestinationName)
		}
	}
	if err := a.prepareCredential("mail-source", &request.Source); err != nil {
		return 0, err
	}
	if err := a.prepareCredential("mail-destination", &request.Destination); err != nil {
		return 0, err
	}
	if err := normalizeMailMappings(request.Mappings); err != nil {
		return 0, err
	}
	id, err := a.migrations.Start(a.ctx, request)
	if err == nil {
		a.discardAllDeletionCredentials()
	}
	return id, err
}

func (a *App) StartJob(request domain.StartJobRequest) (int64, error) {
	if err := a.ensureReady(); err != nil {
		return 0, err
	}
	if !request.MailEnabled && !request.Calendar.Enabled && !request.Contacts.Enabled {
		return 0, errors.New("select at least one service")
	}
	a.protectMailDiagnostics(request.MailSource, request.MailDestination)
	a.protectDAVDiagnostics(request.Calendar.Source, request.Calendar.Destination, request.Contacts.Source, request.Contacts.Destination)
	if a.diagnostics != nil {
		for _, mapping := range request.MailMappings {
			a.diagnostics.Protect(mapping.Source.Name, mapping.DestinationName)
		}
		for _, service := range []domain.DAVServiceRequest{request.Calendar, request.Contacts} {
			for _, mapping := range service.Mappings {
				a.diagnostics.Protect(mapping.Source.Name, mapping.Source.Path, mapping.DestinationName, mapping.DestinationPath)
			}
		}
	}
	if a.diagnostics != nil {
		a.diagnostics.InfoEvent("job_start_requested", diagnostics.Fields{MigrationID: request.MigrationID, Phase: request.Mode, Values: map[string]any{
			"mail": request.MailEnabled, "calendar": request.Calendar.Enabled, "contacts": request.Contacts.Enabled, "concurrency": request.Options.Concurrency,
		}})
	}
	if err := validateMailboxNoticeSettings(&request.MailboxNotices); err != nil {
		return 0, err
	}
	if !request.MailEnabled {
		request.MailboxNotices = domain.MailboxNoticeSettings{}
	}
	if request.Options.Concurrency <= 0 {
		request.Options = domain.DefaultTransferOptions()
	}
	if request.MailEnabled {
		if err := normalizeMailMappings(request.MailMappings); err != nil {
			return 0, err
		}
		if err := a.prepareCredential("mail-source", &request.MailSource); err != nil {
			return 0, err
		}
		if err := a.prepareCredential("mail-destination", &request.MailDestination); err != nil {
			return 0, err
		}
	}
	for _, service := range []*domain.DAVServiceRequest{&request.Calendar, &request.Contacts} {
		if !service.Enabled {
			continue
		}
		if service.Kind != domain.ServiceCalendar && service.Kind != domain.ServiceContacts {
			return 0, fmt.Errorf("invalid DAV service %q", service.Kind)
		}
		if err := a.prepareDAVCredential(string(service.Kind)+"-source", &service.Source); err != nil {
			return 0, err
		}
		if err := a.prepareDAVCredential(string(service.Kind)+"-destination", &service.Destination); err != nil {
			return 0, err
		}
	}
	id := request.MigrationID
	var err error
	if id == 0 {
		id, err = a.db.CreateJob(a.ctx, request)
		if err != nil {
			return 0, err
		}
	} else {
		if request.MailEnabled {
			if err := a.db.UpdateCredentialIDs(a.ctx, id, request.MailSource.CredentialID, request.MailDestination.CredentialID); err != nil {
				return 0, err
			}
		}
		for _, service := range []domain.DAVServiceRequest{request.Calendar, request.Contacts} {
			if service.Enabled {
				if err := a.db.UpdateDAVCredentialIDs(a.ctx, id, service.Kind, service.Source.CredentialID, service.Destination.CredentialID); err != nil {
					return 0, err
				}
			}
		}
	}
	started := make([]domain.ServiceKind, 0, 3)
	if request.MailEnabled {
		_, err = a.migrations.Start(a.ctx, domain.StartRequest{Source: request.MailSource, Destination: request.MailDestination, Mappings: request.MailMappings, Options: request.Options, MigrationID: id, Mode: request.Mode})
		if err != nil {
			_ = a.db.MarkService(context.Background(), id, domain.ServiceMail, domain.MigrationFailed, err.Error())
			return 0, err
		}
		started = append(started, domain.ServiceMail)
	}
	for _, service := range []domain.DAVServiceRequest{request.Calendar, request.Contacts} {
		if !service.Enabled {
			continue
		}
		if err := a.davMigrations.Start(a.ctx, id, service, request.Options, request.Mode); err != nil {
			_ = a.db.MarkService(context.Background(), id, service.Kind, domain.MigrationFailed, err.Error())
			if len(started) > 0 {
				_ = a.CancelJob(id)
			}
			return 0, err
		}
		started = append(started, service.Kind)
	}
	a.discardAllDeletionCredentials()
	if a.diagnostics != nil {
		a.diagnostics.InfoEvent("job_started", diagnostics.Fields{MigrationID: id, Phase: request.Mode, Values: map[string]any{"serviceCount": len(started)}})
	}
	return id, nil
}

func (a *App) ResumeRequirements(migrationID int64) (domain.ResumeRequirements, error) {
	if err := a.ensureReady(); err != nil {
		return domain.ResumeRequirements{}, err
	}
	if migrationID <= 0 {
		return domain.ResumeRequirements{}, errors.New("invalid migration ID")
	}
	if a.diagnostics != nil {
		a.diagnostics.InfoEvent("delta_requirements_requested", diagnostics.Fields{MigrationID: migrationID, Phase: "requirements"})
	}
	migration, err := a.db.RecentByID(a.ctx, migrationID)
	if err != nil {
		return domain.ResumeRequirements{}, err
	}
	services, err := a.db.JobServices(a.ctx, migrationID)
	if err != nil {
		return domain.ResumeRequirements{}, err
	}
	output := domain.ResumeRequirements{Migration: migration, Credentials: make([]domain.CredentialRequirement, 0, len(services))}
	for _, kind := range services {
		switch kind {
		case domain.ServiceMail:
			mailRequest, err := a.db.LoadRequest(a.ctx, migrationID)
			if err != nil {
				return domain.ResumeRequirements{}, err
			}
			output.Credentials = append(output.Credentials, domain.CredentialRequirement{Kind: kind, SourceAvailable: a.credentialAvailable(mailRequest.Source.CredentialID), DestinationAvailable: a.credentialAvailable(mailRequest.Destination.CredentialID)})
		case domain.ServiceCalendar, domain.ServiceContacts:
			service, err := a.db.LoadDAVServiceRequest(a.ctx, migrationID, kind)
			if err != nil {
				return domain.ResumeRequirements{}, err
			}
			output.Credentials = append(output.Credentials, domain.CredentialRequirement{Kind: kind, SourceAvailable: a.credentialAvailable(service.Source.CredentialID), DestinationAvailable: a.credentialAvailable(service.Destination.CredentialID)})
		}
	}
	if a.diagnostics != nil {
		a.diagnostics.InfoEvent("delta_requirements_completed", diagnostics.Fields{MigrationID: migrationID, Phase: "requirements", Values: map[string]any{
			"serviceCount": len(services), "credentialRequirementCount": len(output.Credentials),
		}})
	}
	return output, nil
}

func (a *App) credentialAvailable(id string) bool {
	if id == "" {
		return false
	}
	_, err := a.credentials.Get(id)
	return err == nil
}

func (a *App) ResumeJob(input domain.ResumeJobRequest) (int64, error) {
	if err := a.ensureReady(); err != nil {
		return 0, err
	}
	if input.MigrationID <= 0 {
		return 0, errors.New("invalid migration ID")
	}
	if a.diagnostics != nil {
		a.diagnostics.InfoEvent("delta_start_requested", diagnostics.Fields{MigrationID: input.MigrationID, Phase: "credentials", Values: map[string]any{"credentialServiceCount": len(input.Credentials)}})
	}
	services, err := a.db.JobServices(a.ctx, input.MigrationID)
	if err != nil {
		return 0, err
	}
	request := domain.StartJobRequest{MigrationID: input.MigrationID, Mode: "reconcile", Options: a.db.LoadJobOptions(a.ctx, input.MigrationID)}
	var deletionDestination *domain.AccountConfig
	for _, kind := range services {
		credentials := input.Credentials[kind]
		switch kind {
		case domain.ServiceMail:
			mailRequest, err := a.db.LoadRequest(a.ctx, input.MigrationID)
			if err != nil {
				return 0, err
			}
			a.protectMailDiagnostics(mailRequest.Source, mailRequest.Destination)
			if err := a.resolveResumeAccount("mail-source", &mailRequest.Source, domain.AccountConfig{Password: credentials.SourcePassword, RememberCredential: input.RememberNewCredentials}); err != nil {
				return 0, err
			}
			if err := a.resolveResumeAccount("mail-destination", &mailRequest.Destination, domain.AccountConfig{Password: credentials.DestinationPassword, RememberCredential: input.RememberNewCredentials}); err != nil {
				return 0, err
			}
			if a.diagnostics != nil {
				a.diagnostics.InfoEvent("delta_preflight_started", diagnostics.Fields{MigrationID: input.MigrationID, Service: string(kind), Phase: "preflight"})
			}
			ctx, cancel := context.WithTimeout(a.ctx, 30*time.Minute)
			preflight, preflightErr := a.migrations.Preflight(ctx, mailRequest.Source, mailRequest.Destination)
			cancel()
			if preflightErr != nil {
				if a.diagnostics != nil {
					a.diagnostics.ErrorEvent("delta_preflight_failed", diagnostics.Fields{MigrationID: input.MigrationID, Service: string(kind), Phase: "preflight", ErrorCode: "TB-DIAG-DELTA-PREFLIGHT", Message: preflightErr.Error()})
				}
				return 0, fmt.Errorf("check mailboxes before the delta sync: %w", preflightErr)
			}
			if a.diagnostics != nil {
				a.diagnostics.InfoEvent("delta_preflight_completed", diagnostics.Fields{MigrationID: input.MigrationID, Service: string(kind), Phase: "preflight", Values: map[string]any{
					"sourceFolderCount": len(preflight.Source.Mailboxes), "destinationFolderCount": len(preflight.Destination.Mailboxes), "mappingCount": len(mailRequest.Mappings),
				}})
			}
			a.excludeDeliveredSourceNotice(input.MigrationID, &preflight)
			currentSource := make(map[string]domain.Mailbox, len(preflight.Source.Mailboxes))
			for _, mailbox := range preflight.Source.Mailboxes {
				currentSource[mailbox.Name] = mailbox
			}
			for index := range mailRequest.Mappings {
				if mailbox, ok := currentSource[mailRequest.Mappings[index].Source.Name]; ok {
					mailRequest.Mappings[index].Source = mailbox
				}
			}
			if err := a.db.RefreshMigrationScope(a.ctx, input.MigrationID, mailRequest); err != nil {
				return 0, err
			}
			request.Options = mailRequest.Options
			request.MailEnabled = true
			request.MailSource = mailRequest.Source
			request.MailDestination = mailRequest.Destination
			request.MailMappings = mailRequest.Mappings
			value := mailRequest.Destination
			deletionDestination = &value
		case domain.ServiceCalendar, domain.ServiceContacts:
			service, err := a.db.LoadDAVServiceRequest(a.ctx, input.MigrationID, kind)
			if err != nil {
				return 0, err
			}
			a.protectDAVDiagnostics(service.Source, service.Destination)
			if err := a.resolveDAVResumeEndpoint(string(kind)+"-source", &service.Source, domain.DAVEndpoint{Password: credentials.SourcePassword, RememberCredential: input.RememberNewCredentials}); err != nil {
				return 0, err
			}
			if err := a.resolveDAVResumeEndpoint(string(kind)+"-destination", &service.Destination, domain.DAVEndpoint{Password: credentials.DestinationPassword, RememberCredential: input.RememberNewCredentials}); err != nil {
				return 0, err
			}
			if kind == domain.ServiceCalendar {
				request.Calendar = service
			} else {
				request.Contacts = service
			}
		}
	}
	id, err := a.StartJob(request)
	if err != nil {
		if a.diagnostics != nil {
			a.diagnostics.ErrorEvent("delta_start_failed", diagnostics.Fields{MigrationID: input.MigrationID, Phase: "start", ErrorCode: "TB-DIAG-DELTA-START", Message: err.Error()})
		}
		return 0, err
	}
	if deletionDestination != nil {
		a.deletionMu.Lock()
		a.deletionDestinations[input.MigrationID] = *deletionDestination
		a.deletionMu.Unlock()
	}
	if a.diagnostics != nil {
		a.diagnostics.InfoEvent("delta_started", diagnostics.Fields{MigrationID: id, Phase: "reconcile", Values: map[string]any{"serviceCount": len(services)}})
	}
	return id, nil
}

func (a *App) PauseMigration(id int64) error {
	if err := a.ensureReady(); err != nil {
		return err
	}
	mailErr := a.migrations.Pause(id)
	davErr := a.davMigrations.Pause(id)
	if mailErr != nil && davErr != nil {
		return mailErr
	}
	return nil
}
func (a *App) ContinueMigration(id int64) error {
	if err := a.ensureReady(); err != nil {
		return err
	}
	mailErr := a.migrations.Resume(id)
	davErr := a.davMigrations.Resume(id)
	if mailErr != nil && davErr != nil {
		return mailErr
	}
	return nil
}
func (a *App) CancelMigration(id int64) error {
	if err := a.ensureReady(); err != nil {
		return err
	}
	mailErr := a.migrations.Cancel(id)
	davErr := a.davMigrations.Cancel(id)
	if mailErr != nil && davErr != nil {
		return mailErr
	}
	return nil
}

func (a *App) PauseJob(id int64) error    { return a.PauseMigration(id) }
func (a *App) ContinueJob(id int64) error { return a.ContinueMigration(id) }
func (a *App) CancelJob(id int64) error   { return a.CancelMigration(id) }

func (a *App) ResumeSavedMigration(id int64) (int64, error) {
	return a.resumeMigration(domain.ResumeRequest{MigrationID: id})
}

// ReconcileMigration performs a fresh two-sided preflight before resuming an
// existing migration. Passwords from the visible form are used when present;
// the OS keychain is only a fallback.
func (a *App) ReconcileMigration(input domain.ResumeRequest) (int64, error) {
	return a.resumeMigration(input)
}

func (a *App) resumeMigration(input domain.ResumeRequest) (int64, error) {
	if err := a.ensureReady(); err != nil {
		return 0, err
	}
	if input.MigrationID <= 0 {
		return 0, errors.New("invalid migration id")
	}
	request, err := a.db.LoadRequest(a.ctx, input.MigrationID)
	if err != nil {
		return 0, err
	}
	if err := a.resolveResumeAccount("mail-source", &request.Source, input.Source); err != nil {
		return 0, err
	}
	if err := a.resolveResumeAccount("mail-destination", &request.Destination, input.Destination); err != nil {
		return 0, err
	}
	if input.Options.Concurrency > 0 {
		request.Options = input.Options
	}
	ctx, cancel := context.WithTimeout(a.ctx, 15*time.Minute)
	defer cancel()
	preflight, err := a.migrations.Preflight(ctx, request.Source, request.Destination)
	if err != nil {
		return 0, fmt.Errorf("check mailboxes before reconciliation: %w", err)
	}
	a.excludeDeliveredSourceNotice(input.MigrationID, &preflight)
	currentSource := make(map[string]domain.Mailbox, len(preflight.Source.Mailboxes))
	for _, mailbox := range preflight.Source.Mailboxes {
		currentSource[mailbox.Name] = mailbox
	}
	for index := range request.Mappings {
		if mailbox, ok := currentSource[request.Mappings[index].Source.Name]; ok {
			request.Mappings[index].Source = mailbox
		}
	}
	request.Mode = "reconcile"
	if err := a.db.RefreshMigrationScope(a.ctx, input.MigrationID, request); err != nil {
		return 0, err
	}
	if err := a.db.UpdateCredentialIDs(a.ctx, input.MigrationID, request.Source.CredentialID, request.Destination.CredentialID); err != nil {
		return 0, err
	}
	id, err := a.migrations.Start(a.ctx, request)
	if err != nil {
		return 0, err
	}
	a.discardAllDeletionCredentials()
	a.deletionMu.Lock()
	a.deletionDestinations[input.MigrationID] = request.Destination
	a.deletionMu.Unlock()
	return id, nil
}

func (a *App) excludeDeliveredSourceNotice(migrationID int64, preflight *domain.PreflightResult) {
	if a.db == nil || preflight == nil {
		return
	}
	records, err := a.db.MailboxNoticeRecords(a.ctx, migrationID)
	if err != nil {
		return
	}
	for _, notice := range records {
		if notice.Side != domain.MailboxNoticeSource || notice.Status != "delivered" {
			continue
		}
		for index := range preflight.Source.Mailboxes {
			mailbox := &preflight.Source.Mailboxes[index]
			if !strings.EqualFold(mailbox.Name, "INBOX") || mailbox.Messages == 0 {
				continue
			}
			mailbox.Messages--
			preflight.Source.Messages = max(0, preflight.Source.Messages-1)
			if notice.Size > 0 && mailbox.SizeKnown {
				mailbox.Size = max(0, mailbox.Size-notice.Size)
				preflight.Source.Bytes = max(0, preflight.Source.Bytes-notice.Size)
			}
			return
		}
	}
}

func (a *App) resolveResumeAccount(role string, persisted *domain.AccountConfig, visible domain.AccountConfig) error {
	label := role
	if strings.HasSuffix(role, "source") {
		label = "source"
	} else if strings.HasSuffix(role, "destination") {
		label = "destination"
	}
	if visible.Password != "" {
		if (strings.TrimSpace(visible.Host) != "" && !strings.EqualFold(strings.TrimSpace(visible.Host), strings.TrimSpace(persisted.Host))) || (visible.Username != "" && visible.Username != persisted.Username) {
			return fmt.Errorf("the %s in the connection form does not match this migration", label)
		}
		persisted.Password = visible.Password
		persisted.RememberCredential = visible.RememberCredential
		if visible.RememberCredential {
			return a.prepareCredential(role, persisted)
		}
		return nil
	}
	if persisted.CredentialID == "" {
		return fmt.Errorf("no password is stored for the %s; enter it in the connection form or enable the credential store", label)
	}
	password, err := a.credentials.Get(persisted.CredentialID)
	if err != nil {
		return err
	}
	persisted.Password = password
	persisted.RememberCredential = true
	return nil
}

func (a *App) RecentMigrations() ([]domain.RecentMigration, error) {
	if err := a.ensureReady(); err != nil {
		return nil, err
	}
	return a.db.Recent(a.ctx, 20)
}

func (a *App) JobMailboxNotices(id int64) (domain.MailboxNoticeOverview, error) {
	if err := a.ensureReady(); err != nil {
		return domain.MailboxNoticeOverview{}, err
	}
	if id <= 0 {
		return domain.MailboxNoticeOverview{}, errors.New("invalid migration ID")
	}
	migration, err := a.db.RecentByID(a.ctx, id)
	if err != nil {
		return domain.MailboxNoticeOverview{}, err
	}
	notices, err := a.db.MailboxNoticeStatuses(a.ctx, id)
	if err != nil {
		return domain.MailboxNoticeOverview{}, err
	}
	request, requestErr := a.db.LoadRequest(a.ctx, id)
	overview := domain.MailboxNoticeOverview{
		MigrationID: id, MigrationState: migration.State, Notices: notices,
		Eligible:                  migration.State == domain.MigrationCompleted || migration.State == domain.MigrationCompletedWithErrors,
		RequiresErrorConfirmation: migration.State == domain.MigrationCompletedWithErrors,
	}
	if requestErr == nil {
		overview.SourceCredentialAvailable = a.credentialAvailable(request.Source.CredentialID)
		overview.DestinationCredentialAvailable = a.credentialAvailable(request.Destination.CredentialID)
	}
	return overview, nil
}

func (a *App) FinalizeMailboxNotices(input domain.FinalizeMailboxNoticesRequest) (domain.MailboxNoticeOverview, error) {
	if err := a.ensureReady(); err != nil {
		return domain.MailboxNoticeOverview{}, err
	}
	overview, err := a.JobMailboxNotices(input.MigrationID)
	if err != nil {
		return overview, err
	}
	if !overview.Eligible {
		return overview, errors.New("mailbox notices can only be finalized after a completed migration")
	}
	if overview.RequiresErrorConfirmation && !input.AllowCompletedWithErrors {
		return overview, errors.New("confirm the completed-with-errors warning before finalizing mailbox notices")
	}
	hasPendingSource, hasPendingDestination := false, false
	for _, notice := range overview.Notices {
		if !notice.Enabled || notice.Status == "delivered" {
			continue
		}
		if notice.Side == domain.MailboxNoticeSource {
			hasPendingSource = true
		} else if notice.Side == domain.MailboxNoticeDestination {
			hasPendingDestination = true
		}
	}
	if !hasPendingSource && !hasPendingDestination {
		return overview, nil
	}
	request, err := a.db.LoadRequest(a.ctx, input.MigrationID)
	if err != nil {
		return overview, err
	}
	a.protectMailDiagnostics(request.Source, request.Destination)
	remember := input.RememberNewCredentials
	if hasPendingSource {
		if err := a.resolveResumeAccount("mail-source", &request.Source, domain.AccountConfig{Password: input.SourcePassword, RememberCredential: remember}); err != nil {
			return overview, err
		}
	}
	if hasPendingDestination {
		if err := a.resolveResumeAccount("mail-destination", &request.Destination, domain.AccountConfig{Password: input.DestinationPassword, RememberCredential: remember}); err != nil {
			return overview, err
		}
	}
	if err := a.db.UpdateCredentialIDs(a.ctx, input.MigrationID, request.Source.CredentialID, request.Destination.CredentialID); err != nil {
		return overview, err
	}
	if _, err := a.migrations.FinalizeMailboxNotices(a.ctx, input.MigrationID, request.Source, request.Destination, request.Options); err != nil {
		return overview, err
	}
	return a.JobMailboxNotices(input.MigrationID)
}

func (a *App) JobMailIssues(id int64) ([]domain.MailIssue, error) {
	if err := a.ensureReady(); err != nil {
		return nil, err
	}
	return a.db.MailIssues(a.ctx, id)
}

func (a *App) JobSourceDeletions(id int64) ([]domain.SourceDeletion, error) {
	if err := a.ensureReady(); err != nil {
		return nil, err
	}
	if id <= 0 {
		return nil, errors.New("invalid migration ID")
	}
	return a.db.SourceDeletions(a.ctx, id)
}

func (a *App) ResolveSourceDeletions(request domain.ResolveSourceDeletionsRequest) error {
	if err := a.ensureReady(); err != nil {
		return err
	}
	if request.MigrationID <= 0 || len(request.Actions) == 0 {
		return errors.New("no source deletions selected")
	}
	allKeep := true
	for _, action := range request.Actions {
		if action.Resolution != domain.SourceDeletionKeep {
			allKeep = false
			break
		}
	}
	var destination domain.AccountConfig
	ok := allKeep
	if !allKeep {
		a.deletionMu.Lock()
		destination, ok = a.deletionDestinations[request.MigrationID]
		a.deletionMu.Unlock()
	}
	if !ok {
		stored, err := a.db.LoadRequest(a.ctx, request.MigrationID)
		if err != nil {
			return err
		}
		destination = stored.Destination
		if destination.CredentialID == "" {
			return errors.New("the destination password is no longer in memory; start the delta sync again")
		}
		password, err := a.credentials.Get(destination.CredentialID)
		if err != nil {
			return errors.New("the destination password is unavailable; start the delta sync again")
		}
		destination.Password = password
	}
	if err := a.migrations.ResolveSourceDeletions(a.ctx, request.MigrationID, destination, a.db.LoadJobOptions(a.ctx, request.MigrationID), request.Actions); err != nil {
		return err
	}
	remaining, err := a.db.SourceDeletions(a.ctx, request.MigrationID)
	if err == nil && len(remaining) == 0 {
		a.deletionMu.Lock()
		delete(a.deletionDestinations, request.MigrationID)
		a.deletionMu.Unlock()
	}
	return nil
}

func (a *App) DiscardSourceDeletionCredential(migrationID int64) {
	a.deletionMu.Lock()
	delete(a.deletionDestinations, migrationID)
	a.deletionMu.Unlock()
}

func (a *App) discardAllDeletionCredentials() {
	a.deletionMu.Lock()
	clear(a.deletionDestinations)
	a.deletionMu.Unlock()
}

func (a *App) ResolveMailIssue(id int64, resolution domain.MailIssueResolution) error {
	if err := a.ensureReady(); err != nil {
		return err
	}
	return a.db.ResolveMailIssue(a.ctx, id, resolution)
}

func normalizeMailMappings(mappings []domain.FolderMapping) error {
	for index := range mappings {
		if mappings[index].DestinationExists {
			continue
		}
		mappings[index].DestinationName = folders.NormalizeName(mappings[index].DestinationName)
		if !folders.SafeName(mappings[index].DestinationName) {
			return fmt.Errorf("invalid destination folder %q", mappings[index].DestinationName)
		}
	}
	return nil
}

func (a *App) ExportReport(id int64) (string, error) {
	if err := a.ensureReady(); err != nil {
		return "", err
	}
	report, err := a.db.Report(a.ctx, id)
	if err != nil {
		return "", err
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return "", err
	}
	path, err := runtime.SaveFileDialog(a.ctx, runtime.SaveDialogOptions{Title: "Export migration report", DefaultFilename: fmt.Sprintf("mail-migration-%d.json", id), Filters: []runtime.FileFilter{{DisplayName: "JSON report", Pattern: "*.json"}}})
	if err != nil || path == "" {
		return path, err
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return "", err
	}
	return path, nil
}

func (a *App) prepareCredential(role string, account *domain.AccountConfig) error {
	if !account.RememberCredential {
		account.CredentialID = ""
		return nil
	}
	if account.Password == "" {
		return errors.New("cannot remember an empty password")
	}
	sum := sha256.Sum256([]byte(role + "\x00" + account.Host + "\x00" + account.Username))
	account.CredentialID = hex.EncodeToString(sum[:16])
	if err := a.credentials.Set(account.CredentialID, account.Password); err != nil {
		return fmt.Errorf("save password in operating system credential store: %w", err)
	}
	return nil
}

func (a *App) prepareDAVCredential(role string, endpoint *domain.DAVEndpoint) error {
	if !endpoint.RememberCredential {
		endpoint.CredentialID = ""
		return nil
	}
	if endpoint.Password == "" {
		return errors.New("an empty DAV password cannot be stored")
	}
	sum := sha256.Sum256([]byte(role + "\x00" + endpoint.URL + "\x00" + endpoint.Username))
	endpoint.CredentialID = hex.EncodeToString(sum[:16])
	if err := a.credentials.Set(endpoint.CredentialID, endpoint.Password); err != nil {
		return fmt.Errorf("save DAV password in the system credential store: %w", err)
	}
	return nil
}

func (a *App) resolveDAVResumeEndpoint(role string, persisted *domain.DAVEndpoint, visible domain.DAVEndpoint) error {
	if visible.Password != "" {
		if (visible.Username != "" && visible.Username != persisted.Username) || (strings.TrimSpace(visible.URL) != "" && strings.TrimRight(strings.TrimSpace(visible.URL), "/") != strings.TrimRight(strings.TrimSpace(persisted.URL), "/")) {
			return fmt.Errorf("DAV %s credentials do not match this migration", role)
		}
		persisted.Password = visible.Password
		persisted.RememberCredential = visible.RememberCredential
		if visible.AuthMethod != "" {
			persisted.AuthMethod = visible.AuthMethod
		}
		if visible.RememberCredential {
			return a.prepareDAVCredential(role, persisted)
		}
		return nil
	}
	if persisted.CredentialID == "" {
		return fmt.Errorf("no DAV password is stored for %s", role)
	}
	password, err := a.credentials.Get(persisted.CredentialID)
	if err != nil {
		return err
	}
	persisted.Password = password
	persisted.RememberCredential = true
	return nil
}

func (a *App) JobConflicts(id int64) ([]domain.Conflict, error) {
	if err := a.ensureReady(); err != nil {
		return nil, err
	}
	return a.db.Conflicts(a.ctx, id)
}

func (a *App) ResolveJobConflict(conflictID int64, resolution string) error {
	if err := a.ensureReady(); err != nil {
		return err
	}
	return a.db.ResolveConflict(a.ctx, conflictID, resolution)
}

func (a *App) ExportSupportBundle(id int64) (string, error) {
	if err := a.ensureReady(); err != nil {
		return "", err
	}
	report, err := a.db.Report(a.ctx, id)
	if err != nil {
		return "", err
	}
	conflicts, _ := a.db.Conflicts(a.ctx, id)
	diagnosticSnapshot := ""
	if a.diagnostics != nil {
		diagnosticSnapshot, _ = a.diagnostics.Snapshot(200)
	}
	bundle := struct {
		Version     string            `json:"version"`
		OS          string            `json:"os"`
		Arch        string            `json:"arch"`
		CreatedAt   time.Time         `json:"createdAt"`
		Report      domain.Report     `json:"report"`
		Conflicts   []domain.Conflict `json:"conflicts"`
		Diagnostics string            `json:"diagnostics,omitempty"`
	}{Version: appVersion, OS: goruntime.GOOS, Arch: goruntime.GOARCH, CreatedAt: time.Now().UTC(), Report: report, Conflicts: conflicts, Diagnostics: diagnosticSnapshot}
	data, err := json.MarshalIndent(bundle, "", "  ")
	if err != nil {
		return "", err
	}
	path, err := runtime.SaveFileDialog(a.ctx, runtime.SaveDialogOptions{Title: "Export support bundle", DefaultFilename: fmt.Sprintf("mailmigration-support-%d.json", id), Filters: []runtime.FileFilter{{DisplayName: "JSON", Pattern: "*.json"}}})
	if err != nil || path == "" {
		return path, err
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return "", err
	}
	return path, nil
}
