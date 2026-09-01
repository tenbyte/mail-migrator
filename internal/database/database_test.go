package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tenbyte/mail-migrator/internal/domain"
)

func TestSchemaUpgradeCreatesBackupAndBackfillsMailService(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "state.db")
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	statements := []string{
		`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL)`,
		`INSERT INTO schema_migrations(version,applied_at) VALUES(1,'2026-01-01T00:00:00Z')`,
		`CREATE TABLE migrations (id INTEGER PRIMARY KEY AUTOINCREMENT, created_at TEXT NOT NULL, started_at TEXT, finished_at TEXT, status TEXT NOT NULL, source_host TEXT NOT NULL, destination_host TEXT NOT NULL, source_username TEXT NOT NULL DEFAULT '', destination_username TEXT NOT NULL DEFAULT '', source_port INTEGER NOT NULL DEFAULT 993, destination_port INTEGER NOT NULL DEFAULT 993, source_encryption TEXT NOT NULL DEFAULT 'tls', destination_encryption TEXT NOT NULL DEFAULT 'tls', source_credential_id TEXT, destination_credential_id TEXT, bytes_total INTEGER NOT NULL DEFAULT 0, bytes_copied INTEGER NOT NULL DEFAULT 0, messages_total INTEGER NOT NULL DEFAULT 0, messages_copied INTEGER NOT NULL DEFAULT 0, messages_failed INTEGER NOT NULL DEFAULT 0, last_error TEXT NOT NULL DEFAULT '')`,
		`INSERT INTO migrations(created_at,status,source_host,destination_host,messages_total) VALUES('2026-01-01T00:00:00Z','COMPLETED','old.example','new.example',3)`,
	}
	for _, statement := range statements {
		if _, err := legacy.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := os.Stat(path + ".v1.bak"); err != nil {
		t.Fatalf("schema backup missing: %v", err)
	}
	services, err := db.JobServices(context.Background(), 1)
	if err != nil || len(services) != 1 || services[0] != domain.ServiceMail {
		t.Fatalf("legacy migration was not backfilled: %v, %v", services, err)
	}
}

func TestRemoveStateFilesDeletesDatabaseSidecarsAndBackups(t *testing.T) {
	path := filepath.Join(t.TempDir(), "migrations.db")
	for _, candidate := range []string{path, path + "-wal", path + "-shm", path + ".v1.bak", path + ".v3.bak"} {
		if err := os.WriteFile(candidate, []byte("state"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := RemoveStateFiles(path); err != nil {
		t.Fatal(err)
	}
	for _, candidate := range []string{path, path + "-wal", path + "-shm", path + ".v1.bak", path + ".v3.bak"} {
		if _, err := os.Stat(candidate); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("state file still exists: %s (%v)", candidate, err)
		}
	}
	if err := RemoveStateFiles(path); err != nil {
		t.Fatalf("second reset should be idempotent: %v", err)
	}
}

func TestSchemaV2UpgradeAddsMailVerificationColumns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, column := range []string{"error_code", "destination_sha256", "verification_status", "policy_override", "pre_append_uidnext", "verified_at"} {
		if _, err := legacy.Exec(`ALTER TABLE messages DROP COLUMN ` + column); err != nil {
			t.Fatalf("drop v3 messages column %s: %v", column, err)
		}
	}
	if _, err := legacy.Exec(`ALTER TABLE errors DROP COLUMN code`); err != nil {
		t.Fatalf("drop v3 errors column: %v", err)
	}
	if _, err := legacy.Exec(`DELETE FROM schema_migrations; INSERT INTO schema_migrations(version,applied_at) VALUES(2,'2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := os.Stat(path + ".v2.bak"); err != nil {
		t.Fatalf("schema v2 backup missing: %v", err)
	}
	for _, item := range []struct{ table, column string }{
		{"messages", "error_code"}, {"messages", "destination_sha256"}, {"messages", "verification_status"},
		{"messages", "policy_override"}, {"messages", "pre_append_uidnext"}, {"messages", "verified_at"}, {"errors", "code"},
	} {
		tx, err := db.sql.BeginTx(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		exists, err := columnExists(context.Background(), tx, item.table, item.column)
		_ = tx.Rollback()
		if err != nil || !exists {
			t.Fatalf("missing %s.%s after upgrade: exists=%v err=%v", item.table, item.column, exists, err)
		}
	}
}

func TestRecentEmptySerializesAsArray(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	items, err := db.Recent(context.Background(), 20)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(items)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "[]" {
		t.Fatalf("empty history must be a JSON array, got %s", data)
	}
}

func TestRecentIncludesMailAccountsAndCompletionTime(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	id, err := db.CreateMigration(ctx, domain.StartRequest{
		Source:      domain.AccountConfig{Host: "old.example", Username: "source@example.com", Port: 993, Encryption: domain.EncryptionTLS},
		Destination: domain.AccountConfig{Host: "new.example", Username: "destination@example.com", Port: 993, Encryption: domain.EncryptionTLS},
	})
	if err != nil {
		t.Fatal(err)
	}
	finishedAt := "2026-09-01T14:05:06Z"
	if _, err := db.sql.ExecContext(ctx, `UPDATE migrations SET status=?,finished_at=? WHERE id=?`, domain.MigrationCompleted, finishedAt, id); err != nil {
		t.Fatal(err)
	}
	items, err := db.Recent(ctx, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("expected one recent migration, got %d", len(items))
	}
	item := items[0]
	if item.SourceUsername != "source@example.com" || item.DestinationUsername != "destination@example.com" {
		t.Fatalf("recent migration omitted account identities: %#v", item)
	}
	if item.FinishedAt == nil || item.FinishedAt.Format(time.RFC3339) != finishedAt {
		t.Fatalf("recent migration omitted completion time: %#v", item.FinishedAt)
	}
}

func TestResumeIdentityAndProgressAreAtomic(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	req := domain.StartRequest{Source: domain.AccountConfig{Host: "old", Port: 993, Encryption: domain.EncryptionTLS}, Destination: domain.AccountConfig{Host: "new", Port: 993, Encryption: domain.EncryptionTLS}, Mappings: []domain.FolderMapping{{Source: domain.Mailbox{Name: "INBOX", Delimiter: '/', Messages: 1, Size: 42, UIDValidity: 7, Selectable: true}, DestinationName: "INBOX", Enabled: true}}}
	id, err := db.CreateMigration(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	fs, err := db.Folders(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	skipped, err := db.BeginMessage(ctx, id, fs[0].ID, 7, 10, 42, "<x>", time.Now())
	if err != nil || skipped {
		t.Fatalf("first begin skipped=%v err=%v", skipped, err)
	}
	if err := db.CompleteMessage(ctx, id, fs[0].ID, 7, 10, 99, 42, "abc"); err != nil {
		t.Fatal(err)
	}
	skipped, err = db.BeginMessage(ctx, id, fs[0].ID, 7, 10, 42, "<x>", time.Now())
	if err != nil || !skipped {
		t.Fatalf("resume identity failed skipped=%v err=%v", skipped, err)
	}
	m, err := db.RecentByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if m.MessagesCopied != 1 || m.BytesCopied != 42 {
		t.Fatalf("bad progress: %#v", m)
	}
}

func TestMailVerificationModePersistsAndLegacyFalseStaysMetadata(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	request := domain.StartRequest{
		Source: domain.AccountConfig{Host: "old", Port: 993, Encryption: domain.EncryptionTLS}, Destination: domain.AccountConfig{Host: "new", Port: 993, Encryption: domain.EncryptionTLS},
		Mappings: []domain.FolderMapping{{Source: domain.Mailbox{Name: "INBOX", UIDValidity: 7, Selectable: true}, DestinationName: "INBOX", Enabled: true}},
		Options:  domain.DefaultTransferOptions(),
	}
	migrationID, err := db.CreateMigration(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := db.LoadRequest(ctx, migrationID)
	if err != nil || loaded.Options.VerificationMode != domain.VerificationFullHash {
		t.Fatalf("full-hash option was not persisted: %#v, %v", loaded.Options, err)
	}
	if _, err := db.sql.ExecContext(ctx, `UPDATE migration_services SET options_json='{"verifyAfter":false}' WHERE migration_id=? AND kind=?`, migrationID, domain.ServiceMail); err != nil {
		t.Fatal(err)
	}
	loaded, err = db.LoadRequest(ctx, migrationID)
	if err != nil || loaded.Options.VerifyAfter || loaded.Options.VerificationMode != domain.VerificationMetadata {
		t.Fatalf("legacy metadata verification was not preserved: %#v, %v", loaded.Options, err)
	}
}

func TestLegacyByteTotalsCredentialFlagsAndWarningDetailsAreRecovered(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	request := domain.StartRequest{
		Source:      domain.AccountConfig{Host: "old", Port: 993, Encryption: domain.EncryptionTLS},
		Destination: domain.AccountConfig{Host: "new", Port: 993, Encryption: domain.EncryptionTLS},
		Mappings:    []domain.FolderMapping{{Source: domain.Mailbox{Name: "INBOX", UIDValidity: 7, Messages: 1, Selectable: true}, DestinationName: "INBOX", Enabled: true}},
		Options:     domain.DefaultTransferOptions(),
	}
	migrationID, err := db.CreateMigration(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	folders, err := db.Folders(ctx, migrationID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.BeginMessage(ctx, migrationID, folders[0].ID, 7, 10, 42, "<message>", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := db.CompleteMessage(ctx, migrationID, folders[0].ID, 7, 10, 99, 42, "hash"); err != nil {
		t.Fatal(err)
	}
	if err := db.AddMessageWarning(ctx, migrationID, folders[0].ID, 10, "TB-MAIL-SOURCE-NO-MESSAGE-ID", "Beispielwarnung"); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateCredentialIDs(ctx, migrationID, "source-id", "destination-id"); err != nil {
		t.Fatal(err)
	}
	loaded, err := db.LoadRequest(ctx, migrationID)
	if err != nil || !loaded.Source.RememberCredential || !loaded.Destination.RememberCredential {
		t.Fatalf("stored credential flags were not restored: %#v, %v", loaded, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	migration, err := db.RecentByID(ctx, migrationID)
	if err != nil || migration.BytesTotal != 42 {
		t.Fatalf("legacy byte total was not recovered: %#v, %v", migration, err)
	}
	report, err := db.Report(ctx, migrationID)
	if err != nil || report.Warnings != 1 || len(report.WarningDetails) != 1 {
		t.Fatalf("warning details missing from report: %#v, %v", report, err)
	}
	warning := report.WarningDetails[0]
	if warning.Folder != "INBOX" || warning.SourceUID != 10 || warning.Code != "TB-MAIL-SOURCE-NO-MESSAGE-ID" || warning.Message != "Beispielwarnung" {
		t.Fatalf("unexpected warning detail: %#v", warning)
	}
}

func TestFolderLevelServiceErrorAppearsInReport(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	request := domain.StartRequest{
		Source:      domain.AccountConfig{Host: "old", Port: 993, Encryption: domain.EncryptionTLS},
		Destination: domain.AccountConfig{Host: "new", Port: 993, Encryption: domain.EncryptionTLS},
		Mappings:    []domain.FolderMapping{{Source: domain.Mailbox{Name: "INBOX", UIDValidity: 7, Messages: 1, Selectable: true}, DestinationName: "INBOX", Enabled: true}},
		Options:     domain.DefaultTransferOptions(),
	}
	migrationID, err := db.CreateMigration(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	folders, err := db.Folders(ctx, migrationID)
	if err != nil {
		t.Fatal(err)
	}
	message := "Zielordner konnte nicht inventarisiert werden"
	if err := db.AddServiceError(ctx, migrationID, folders[0].ID, domain.ServiceMail, "TB-MAIL-DUPLICATE-INVENTORY", message); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkMigration(ctx, migrationID, domain.MigrationCompletedWithErrors, ""); err != nil {
		t.Fatal(err)
	}
	report, err := db.Report(ctx, migrationID)
	if err != nil {
		t.Fatal(err)
	}
	if report.Errors != 1 || len(report.ErrorDetails) != 1 {
		t.Fatalf("folder-level error missing from report: %#v", report)
	}
	detail := report.ErrorDetails[0]
	if detail.Folder != "INBOX" || detail.SourceUID != 0 || detail.Code != "TB-MAIL-DUPLICATE-INVENTORY" || detail.Message != message {
		t.Fatalf("unexpected folder-level error detail: %#v", detail)
	}
	if len(report.Services) != 1 || report.Services[0].LastError != message {
		t.Fatalf("service last error was cleared: %#v", report.Services)
	}
}

func TestMissingDestinationCopyIsRequeuedAtomically(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	request := domain.StartRequest{
		Source:      domain.AccountConfig{Host: "old", Port: 993, Encryption: domain.EncryptionTLS},
		Destination: domain.AccountConfig{Host: "new", Port: 993, Encryption: domain.EncryptionTLS},
		Mappings:    []domain.FolderMapping{{Source: domain.Mailbox{Name: "INBOX", Delimiter: '/', Messages: 1, Size: 42, UIDValidity: 7, Selectable: true}, DestinationName: "INBOX", Enabled: true}},
	}
	migrationID, err := db.CreateMigration(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	folders, err := db.Folders(ctx, migrationID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.BeginMessage(ctx, migrationID, folders[0].ID, 7, 10, 42, "<message>", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := db.CompleteMessage(ctx, migrationID, folders[0].ID, 7, 10, 99, 42, "hash"); err != nil {
		t.Fatal(err)
	}
	copied, err := db.CopiedMessages(ctx, migrationID, folders[0].ID, 7)
	if err != nil || len(copied) != 1 || copied[0].DestinationUID != 99 {
		t.Fatalf("unexpected copied records: %#v, %v", copied, err)
	}
	requeued, size, err := db.RequeueCopiedMessage(ctx, migrationID, copied[0].ID)
	if err != nil || !requeued || size != 42 {
		t.Fatalf("requeue=%v size=%d err=%v", requeued, size, err)
	}
	unfinished, err := db.UnfinishedUIDs(ctx, migrationID, folders[0].ID, 7)
	if err != nil || len(unfinished) != 1 || unfinished[0] != 10 {
		t.Fatalf("unexpected unfinished UIDs: %v, %v", unfinished, err)
	}
	summary, err := db.RecentByID(ctx, migrationID)
	if err != nil || summary.MessagesCopied != 0 || summary.BytesCopied != 0 {
		t.Fatalf("progress was not rolled back: %#v, %v", summary, err)
	}
}

func TestEmptyMailQuarantineRequiresOneExplicitRelease(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	request := domain.StartRequest{
		Source: domain.AccountConfig{Host: "old", Port: 993, Encryption: domain.EncryptionTLS}, Destination: domain.AccountConfig{Host: "new", Port: 993, Encryption: domain.EncryptionTLS},
		Mappings: []domain.FolderMapping{{Source: domain.Mailbox{Name: "INBOX", UIDValidity: 7, Messages: 1, Selectable: true}, DestinationName: "INBOX", DestinationExists: true, Enabled: true}},
	}
	migrationID, err := db.CreateMigration(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	folders, err := db.Folders(ctx, migrationID)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.RecordMessageIssue(ctx, migrationID, folders[0].ID, 7, 10, 0, "", time.Now(), domain.MessageQuarantined, "TB-MAIL-SOURCE-EMPTY", "Leere Quellmail"); err != nil {
		t.Fatal(err)
	}
	unfinished, err := db.UnfinishedUIDs(ctx, migrationID, folders[0].ID, 7)
	if err != nil || len(unfinished) != 0 {
		t.Fatalf("quarantined mail must not be retried automatically: %v, %v", unfinished, err)
	}
	issues, err := db.MailIssues(ctx, migrationID)
	if err != nil || len(issues) != 1 || len(issues[0].AllowedActions) != 2 || issues[0].AllowedActions[0] != domain.MailIssueTransferAnyway {
		t.Fatalf("unexpected quarantine actions: %#v, %v", issues, err)
	}
	if err := db.ResolveMailIssue(ctx, issues[0].ID, domain.MailIssueTransferAnyway); err != nil {
		t.Fatal(err)
	}
	unfinished, err = db.UnfinishedUIDs(ctx, migrationID, folders[0].ID, 7)
	if err != nil || len(unfinished) != 1 || unfinished[0] != 10 {
		t.Fatalf("released mail was not queued exactly once: %v, %v", unfinished, err)
	}
	skipped, err := db.BeginMessage(ctx, migrationID, folders[0].ID, 7, 10, 0, "", time.Now())
	if err != nil || skipped {
		t.Fatalf("released mail could not begin: skipped=%v err=%v", skipped, err)
	}
	record, err := db.MessageTransfer(ctx, migrationID, folders[0].ID, 7, 10)
	if err != nil || record.PolicyOverride != string(domain.MailIssueTransferAnyway) {
		t.Fatalf("explicit release was not preserved: %#v, %v", record, err)
	}
	if err := db.CompleteVerifiedMessage(ctx, migrationID, folders[0].ID, 7, 10, 99, 0, "empty-hash", "empty-hash", "verified"); err != nil {
		t.Fatal(err)
	}
	skipped, err = db.BeginMessage(ctx, migrationID, folders[0].ID, 7, 10, 0, "", time.Now())
	if err != nil || !skipped {
		t.Fatalf("verified release could be transferred twice: skipped=%v err=%v", skipped, err)
	}
}

func TestCrashRecoveryNeverBlindlyRetriesAppend(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	request := domain.StartRequest{
		Source: domain.AccountConfig{Host: "old", Port: 993, Encryption: domain.EncryptionTLS}, Destination: domain.AccountConfig{Host: "new", Port: 993, Encryption: domain.EncryptionTLS},
		Mappings: []domain.FolderMapping{{Source: domain.Mailbox{Name: "INBOX", UIDValidity: 7, Messages: 2, Selectable: true}, DestinationName: "INBOX", DestinationExists: true, Enabled: true}},
	}
	migrationID, err := db.CreateMigration(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	folders, err := db.Folders(ctx, migrationID)
	if err != nil {
		t.Fatal(err)
	}
	for _, uid := range []uint32{10, 11} {
		if _, err := db.BeginMessage(ctx, migrationID, folders[0].ID, 7, uid, 42, "", time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.SetMessageTransferIdentity(ctx, migrationID, folders[0].ID, 10, "", 100); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkMessageVerifying(ctx, migrationID, folders[0].ID, 11, 101, "source-hash"); err != nil {
		t.Fatal(err)
	}
	if err := db.RecoverMigrationMessages(ctx, migrationID); err != nil {
		t.Fatal(err)
	}
	uncertain, err := db.MessageTransfer(ctx, migrationID, folders[0].ID, 7, 10)
	if err != nil || uncertain.State != domain.MessageUnknown || uncertain.PreAppendUIDNext != 100 {
		t.Fatalf("in-flight APPEND was not isolated: %#v, %v", uncertain, err)
	}
	verifying, err := db.MessageTransfer(ctx, migrationID, folders[0].ID, 7, 11)
	if err != nil || verifying.State != domain.MessagePending || verifying.PolicyOverride != string(domain.MailIssueVerifyAgain) || verifying.DestinationUID != 101 {
		t.Fatalf("verification recovery was not constrained to the existing target: %#v, %v", verifying, err)
	}
	unfinished, err := db.UnfinishedUIDs(ctx, migrationID, folders[0].ID, 7)
	if err != nil || len(unfinished) != 2 || unfinished[0] != 10 || unfinished[1] != 11 {
		t.Fatalf("recovery items were not available for safe inspection: %v, %v", unfinished, err)
	}
}

func TestDAVRepairConflictAndProgressRemainConsistent(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	request := domain.StartJobRequest{
		Calendar: domain.DAVServiceRequest{Kind: domain.ServiceCalendar, Enabled: true, Source: domain.DAVEndpoint{URL: "https://old.example/dav", Username: "old"}, Destination: domain.DAVEndpoint{URL: "https://new.example/dav", Username: "new"}, Mappings: []domain.CollectionMapping{{Source: domain.DAVCollection{Path: "/old/cal/", Name: "Kalender", Kind: domain.ServiceCalendar, Objects: 1, Bytes: 42}, DestinationPath: "/new/cal/", DestinationName: "Kalender", DestinationExists: true, Enabled: true}}},
		Options:  domain.DefaultTransferOptions(),
	}
	id, err := db.CreateJob(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	collections, err := db.DAVCollections(ctx, id, domain.ServiceCalendar)
	if err != nil || len(collections) != 1 {
		t.Fatalf("collections=%#v err=%v", collections, err)
	}
	resource, err := db.UpsertDAVResource(ctx, id, collections[0].ID, domain.ServiceCalendar, "/old/cal/one.ics", "one", "source-1", 42)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.BeginDAVResource(ctx, resource.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.CompleteDAVResource(ctx, id, resource.ID, "hash", "/new/cal/one.ics", "destination-1", 42, false); err != nil {
		t.Fatal(err)
	}
	progress, _ := db.ServiceProgresses(ctx, id)
	if len(progress) != 1 || progress[0].ItemsDone != 1 || progress[0].BytesDone != 42 {
		t.Fatalf("unexpected completed progress: %#v", progress)
	}
	if err := db.ResetDAVInventory(ctx, collections[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := db.RequeueMissingDAVTargets(ctx, id, collections[0].ID); err != nil {
		t.Fatal(err)
	}
	progress, _ = db.ServiceProgresses(ctx, id)
	if progress[0].ItemsDone != 0 || progress[0].BytesDone != 0 {
		t.Fatalf("repair queue did not roll progress back: %#v", progress[0])
	}
	resource, err = db.DAVResourceByHref(ctx, id, collections[0].ID, "/old/cal/one.ics")
	if err != nil || resource.State != domain.MessagePending {
		t.Fatalf("resource was not queued for repair: %#v, %v", resource, err)
	}
	if err := db.AddConflict(ctx, id, resource.ID, domain.ServiceCalendar, "source-1", "destination-user-edit"); err != nil {
		t.Fatal(err)
	}
	conflicts, err := db.Conflicts(ctx, id)
	if err != nil || len(conflicts) != 1 {
		t.Fatalf("conflicts=%#v err=%v", conflicts, err)
	}
	if err := db.ResolveConflict(ctx, conflicts[0].ID, "destination"); err != nil {
		t.Fatal(err)
	}
	resource, _ = db.DAVResourceByHref(ctx, id, collections[0].ID, "/old/cal/one.ics")
	if resource.State != domain.MessageSkipped {
		t.Fatalf("destination decision not persisted: %#v", resource)
	}
}
