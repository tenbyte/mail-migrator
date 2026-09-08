package migration

import (
	"bytes"
	"context"
	"fmt"
	"time"

	"github.com/tenbyte/mail-migrator/internal/database"
	"github.com/tenbyte/mail-migrator/internal/domain"
	"github.com/tenbyte/mail-migrator/internal/mailimap"
	"github.com/tenbyte/mail-migrator/internal/security"
)

func (s *Service) FinalizeMailboxNotices(ctx context.Context, migrationID int64, source, destination domain.AccountConfig, options domain.TransferOptions) ([]domain.MailboxNoticeStatus, error) {
	records, err := s.db.MailboxNoticeRecords(ctx, migrationID)
	if err != nil {
		return nil, err
	}
	for _, record := range records {
		if !record.Enabled || record.Status == "delivered" {
			continue
		}
		account := source
		if record.Side == domain.MailboxNoticeDestination {
			account = destination
		}
		if account.Password == "" {
			_ = s.db.FailMailboxNotice(ctx, record.ID, "The mailbox password is unavailable.")
			continue
		}
		if err := s.db.BeginMailboxNotice(ctx, record.ID); err != nil {
			return nil, err
		}
		if err := s.deliverMailboxNotice(ctx, record, account, options); err != nil {
			message := security.RedactError(err.Error(), account.Password)
			_ = s.db.FailMailboxNotice(context.Background(), record.ID, security.SanitizeLogValue(message))
		}
	}
	return s.db.MailboxNoticeStatuses(ctx, migrationID)
}

func (s *Service) deliverMailboxNotice(ctx context.Context, record database.MailboxNoticeRecord, account domain.AccountConfig, options domain.TransferOptions) error {
	timeout := time.Duration(options.ConnectionTimeout) * time.Second
	stall := time.Duration(options.StallTimeout) * time.Second
	client, err := s.factory.Connect(ctx, account, timeout, stall)
	if err != nil {
		return err
	}
	defer client.Close()

	createdAt := time.Now().UTC()
	raw, err := mailimap.BuildMailboxNotice(record.Side, record.CustomText, account.Username, record.MessageID, createdAt)
	if err != nil {
		return err
	}
	uidValidity, existingUID, err := client.FindMessageByID(ctx, "INBOX", record.MessageID)
	if err != nil {
		return err
	}
	if existingUID != 0 {
		return s.db.CompleteMailboxNotice(ctx, record.ID, uidValidity, existingUID, int64(len(raw)))
	}

	meta := mailimap.MessageMetadata{InternalDate: createdAt, Size: int64(len(raw)), SizeKnown: true, MessageID: record.MessageID}
	result, appendErr := client.AppendMessage(ctx, "INBOX", meta, bytes.NewReader(raw), nil, nil)
	if appendErr != nil || result.UID == 0 {
		foundValidity, foundUID, findErr := client.FindMessageByID(ctx, "INBOX", record.MessageID)
		if findErr == nil && foundUID != 0 {
			return s.db.CompleteMailboxNotice(ctx, record.ID, foundValidity, foundUID, int64(len(raw)))
		}
		if appendErr != nil {
			return appendErr
		}
		if findErr != nil {
			return fmt.Errorf("the notice was appended without a UID and could not be verified: %w", findErr)
		}
		return fmt.Errorf("the notice was appended without a UID and could not be found by Message-ID")
	}
	if result.UIDValidity == 0 {
		result.UIDValidity = uidValidity
	}
	return s.db.CompleteMailboxNotice(ctx, record.ID, result.UIDValidity, result.UID, int64(len(raw)))
}
