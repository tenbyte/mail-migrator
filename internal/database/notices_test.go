package database

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tenbyte/mail-migrator/internal/domain"
)

func TestMailboxNoticeSettingsAndJobSnapshot(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	settings := domain.MailboxNoticeSettings{
		Source:      domain.MailboxNoticeTemplate{Enabled: true, CustomText: "Old server details"},
		Destination: domain.MailboxNoticeTemplate{Enabled: true, CustomText: "Contact the help desk"},
	}
	if err := db.SaveMailboxNoticeSettings(ctx, settings); err != nil {
		t.Fatal(err)
	}
	loaded, err := db.MailboxNoticeSettings(ctx)
	if err != nil || loaded != settings {
		t.Fatalf("settings=%#v err=%v", loaded, err)
	}
	request := domain.StartJobRequest{
		MailEnabled:     true,
		MailSource:      domain.AccountConfig{Host: "old.example", Port: 993, Encryption: domain.EncryptionTLS, Username: "old@example.com"},
		MailDestination: domain.AccountConfig{Host: "new.example", Port: 993, Encryption: domain.EncryptionTLS, Username: "new@example.com"},
		Options:         domain.DefaultTransferOptions(), MailboxNotices: settings,
	}
	id, err := db.CreateJob(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	settings.Source.CustomText = "Changed globally"
	if err := db.SaveMailboxNoticeSettings(ctx, settings); err != nil {
		t.Fatal(err)
	}
	records, err := db.MailboxNoticeRecords(ctx, id)
	if err != nil || len(records) != 2 {
		t.Fatalf("records=%#v err=%v", records, err)
	}
	for _, record := range records {
		if !record.Enabled || record.Status != "pending" || !strings.HasPrefix(record.MessageID, "<tbmm-") || !strings.HasSuffix(record.MessageID, "@localhost>") {
			t.Fatalf("unexpected snapshot record: %#v", record)
		}
	}
	if records[1].Side != domain.MailboxNoticeSource || records[1].CustomText != "Old server details" {
		t.Fatalf("source snapshot changed with global settings: %#v", records[1])
	}
	if records[0].MessageID == records[1].MessageID {
		t.Fatal("source and destination notices must have distinct message IDs")
	}
	if err := db.CompleteMailboxNotice(ctx, records[1].ID, 17, 42, 1234); err != nil {
		t.Fatal(err)
	}
	ids, err := db.SourceMailboxNoticeMessageIDs(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := ids[strings.ToLower(records[1].MessageID)]; !ok {
		t.Fatalf("source notice message ID missing: %#v", ids)
	}
	statuses, err := db.MailboxNoticeStatuses(ctx, id)
	if err != nil || statuses[1].Status != "delivered" || statuses[1].DeliveredAt == nil {
		t.Fatalf("statuses=%#v err=%v", statuses, err)
	}
}
