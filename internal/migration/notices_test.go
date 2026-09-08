package migration

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tenbyte/mail-migrator/internal/database"
	"github.com/tenbyte/mail-migrator/internal/domain"
)

func TestFinalizeMailboxNoticesIsUnreadDestinationFirstAndIdempotent(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	request := domain.StartJobRequest{
		MailEnabled:     true,
		MailSource:      domain.AccountConfig{Host: "source", Port: 993, Encryption: domain.EncryptionTLS, Username: "source@example.com", Password: "source-secret"},
		MailDestination: domain.AccountConfig{Host: "destination", Port: 993, Encryption: domain.EncryptionTLS, Username: "destination@example.com", Password: "destination-secret"},
		Options:         domain.DefaultTransferOptions(),
		MailboxNotices: domain.MailboxNoticeSettings{
			Source:      domain.MailboxNoticeTemplate{Enabled: true, CustomText: "Use the new server."},
			Destination: domain.MailboxNoticeTemplate{Enabled: true, CustomText: "Review all folders."},
		},
	}
	id, err := db.CreateJob(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	destination := &noticeClient{reconciliationClient: &reconciliationClient{uidValidity: 22}, appendUID: 101}
	source := &noticeClient{reconciliationClient: &reconciliationClient{uidValidity: 11}, existingUID: 77}
	var order []string
	service := New(db, noticeFactory{clients: map[string]*noticeClient{"source": source, "destination": destination}, order: &order}, nil)
	statuses, err := service.FinalizeMailboxNotices(ctx, id, request.MailSource, request.MailDestination, request.Options)
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 2 || statuses[0].Side != domain.MailboxNoticeDestination || statuses[0].Status != "delivered" || statuses[1].Side != domain.MailboxNoticeSource || statuses[1].Status != "delivered" {
		t.Fatalf("unexpected statuses: %#v", statuses)
	}
	if destination.appendCalls != 1 || source.appendCalls != 0 {
		t.Fatalf("append calls destination=%d source=%d", destination.appendCalls, source.appendCalls)
	}
	if len(order) < 2 || order[0] != "destination" || order[1] != "source" {
		t.Fatalf("mailboxes were not finalized destination first: %v", order)
	}
	if len(destination.appendFlags) != 1 || len(destination.appendFlags[0]) != 0 {
		t.Fatalf("notice was appended with flags: %#v", destination.appendFlags)
	}
	if !strings.Contains(string(destination.appended[0]), domain.MailboxNoticeDestinationSubject) {
		t.Fatal("destination notice body was not appended")
	}
	if _, err := service.FinalizeMailboxNotices(ctx, id, request.MailSource, request.MailDestination, request.Options); err != nil {
		t.Fatal(err)
	}
	if destination.appendCalls != 1 || source.appendCalls != 0 {
		t.Fatalf("retry created a duplicate: destination=%d source=%d", destination.appendCalls, source.appendCalls)
	}
}

func TestFinalizeMailboxNoticeReconcilesUncertainAppendByMessageID(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	request := domain.StartJobRequest{
		MailEnabled:    true,
		MailSource:     domain.AccountConfig{Host: "source", Port: 993, Encryption: domain.EncryptionTLS, Username: "source@example.com", Password: "secret"},
		Options:        domain.DefaultTransferOptions(),
		MailboxNotices: domain.MailboxNoticeSettings{Source: domain.MailboxNoticeTemplate{Enabled: true}},
	}
	id, err := db.CreateJob(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	source := &noticeClient{reconciliationClient: &reconciliationClient{uidValidity: 33}, findUIDs: []uint32{0, 88}, appendErr: errors.New("connection closed after APPEND")}
	service := New(db, noticeFactory{clients: map[string]*noticeClient{"source": source}}, nil)
	statuses, err := service.FinalizeMailboxNotices(ctx, id, request.MailSource, request.MailDestination, request.Options)
	if err != nil {
		t.Fatal(err)
	}
	if source.appendCalls != 1 || len(statuses) != 2 || statuses[1].Status != "delivered" {
		t.Fatalf("uncertain APPEND was not reconciled: calls=%d statuses=%#v", source.appendCalls, statuses)
	}
	records, err := db.MailboxNoticeRecords(ctx, id)
	if err != nil || records[1].UID != 88 || records[1].UIDValidity != 33 {
		t.Fatalf("reconciled identity was not stored: %#v, %v", records, err)
	}
}
