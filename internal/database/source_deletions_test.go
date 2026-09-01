package database

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/tenbyte/mail-migrator/internal/domain"
)

func TestSourceDeletionDecisionsPersistAndAreReported(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	request := domain.StartRequest{
		Source:      domain.AccountConfig{Host: "source", Port: 993, Encryption: domain.EncryptionTLS},
		Destination: domain.AccountConfig{Host: "destination", Port: 993, Encryption: domain.EncryptionTLS},
		Mappings:    []domain.FolderMapping{{Source: domain.Mailbox{Name: "Entwürfe", UIDValidity: 7, Messages: 4, Selectable: true}, DestinationName: "草稿📨", Enabled: true}},
	}
	migrationID, err := db.CreateMigration(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	folderRows, err := db.Folders(ctx, migrationID)
	if err != nil {
		t.Fatal(err)
	}
	folder := folderRows[0]
	date := time.Now().UTC().Truncate(time.Second)
	for index, uid := range []uint32{10, 11, 12, 13} {
		if _, err := db.BeginMessage(ctx, migrationID, folder.ID, 7, uid, int64(40+index), "<message>", date); err != nil {
			t.Fatal(err)
		}
		if err := db.CompleteMessage(ctx, migrationID, folder.ID, 7, uid, 100+uid, int64(40+index), "hash"); err != nil {
			t.Fatal(err)
		}
	}
	copied, err := db.CopiedMessages(ctx, migrationID, folder.ID, 7)
	if err != nil {
		t.Fatal(err)
	}
	for index, record := range copied {
		if err := db.UpsertSourceDeletion(ctx, migrationID, record.ID, record.DestinationUID, 9, "Betreff", "sender@example.test"); err != nil {
			t.Fatal(err)
		}
		if index == 3 {
			break
		}
	}
	items, err := db.SourceDeletions(ctx, migrationID)
	if err != nil || len(items) != 4 {
		t.Fatalf("unexpected deletion inventory: %#v, %v", items, err)
	}
	if items[0].Folder != "Entwürfe" || items[0].DestinationFolder != "草稿📨" || items[0].Subject != "Betreff" || items[0].From != "sender@example.test" {
		t.Fatalf("display metadata was not persisted: %#v", items[0])
	}
	if err := db.ResolveSourceDeletionKeep(ctx, migrationID, items[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := db.CompleteSourceDeletion(ctx, migrationID, items[1].ID, domain.SourceDeletionTrash); err != nil {
		t.Fatal(err)
	}
	if err := db.CompleteSourceDeletion(ctx, migrationID, items[2].ID, domain.SourceDeletionDelete); err != nil {
		t.Fatal(err)
	}
	if err := db.FailSourceDeletion(ctx, migrationID, items[3].ID, domain.SourceDeletionDelete, "UIDVALIDITY nicht eindeutig"); err != nil {
		t.Fatal(err)
	}

	// Re-detection must not reopen a remembered keep decision, and cleanup of
	// an absent target must not erase completed audit rows.
	if err := db.UpsertSourceDeletion(ctx, migrationID, copied[0].ID, copied[0].DestinationUID, 9, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := db.ClearUnresolvedSourceDeletion(ctx, copied[1].ID); err != nil {
		t.Fatal(err)
	}
	remaining, err := db.SourceDeletions(ctx, migrationID)
	if err != nil || len(remaining) != 1 || remaining[0].ID != items[3].ID || remaining[0].LastError == "" {
		t.Fatalf("only the failed decision should remain actionable: %#v, %v", remaining, err)
	}
	report, err := db.Report(ctx, migrationID)
	if err != nil {
		t.Fatal(err)
	}
	if report.SourceDeletionsKept != 1 || report.SourceDeletionsTrashed != 1 || report.SourceDeletionsDeleted != 1 || report.SourceDeletionErrors != 1 {
		t.Fatalf("unexpected deletion report counters: %#v", report)
	}
}
