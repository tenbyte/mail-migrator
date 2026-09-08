package database

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/tenbyte/mail-migrator/internal/domain"
)

const mailboxNoticeSettingsKey = "mailbox_notice_settings"

type MailboxNoticeRecord struct {
	ID          int64
	MigrationID int64
	Side        domain.MailboxNoticeSide
	Enabled     bool
	CustomText  string
	Subject     string
	MessageID   string
	Status      string
	UID         uint32
	UIDValidity uint32
	Size        int64
	LastError   string
	DeliveredAt *time.Time
}

func (d *DB) MailboxNoticeSettings(ctx context.Context) (domain.MailboxNoticeSettings, error) {
	var raw string
	err := d.sql.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=?`, mailboxNoticeSettingsKey).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.MailboxNoticeSettings{}, nil
	}
	if err != nil {
		return domain.MailboxNoticeSettings{}, err
	}
	var settings domain.MailboxNoticeSettings
	if err := json.Unmarshal([]byte(raw), &settings); err != nil {
		return settings, fmt.Errorf("decode mailbox notice settings: %w", err)
	}
	return settings, nil
}

func (d *DB) SaveMailboxNoticeSettings(ctx context.Context, settings domain.MailboxNoticeSettings) error {
	raw, err := json.Marshal(settings)
	if err != nil {
		return err
	}
	_, err = d.sql.ExecContext(ctx, `INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, mailboxNoticeSettingsKey, string(raw))
	return err
}

func configureMailboxNotices(ctx context.Context, tx *sql.Tx, migrationID int64, settings domain.MailboxNoticeSettings) error {
	entries := []struct {
		side     domain.MailboxNoticeSide
		template domain.MailboxNoticeTemplate
		subject  string
	}{
		{domain.MailboxNoticeSource, settings.Source, domain.MailboxNoticeSourceSubject},
		{domain.MailboxNoticeDestination, settings.Destination, domain.MailboxNoticeDestinationSubject},
	}
	for _, entry := range entries {
		messageID, err := newMailboxNoticeMessageID()
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO mailbox_notices(migration_id,side,enabled,custom_text,subject,message_id,status,created_at) VALUES(?,?,?,?,?,?,?,?)`, migrationID, entry.side, entry.template.Enabled, entry.template.CustomText, entry.subject, messageID, "pending", time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			return err
		}
	}
	return nil
}

func newMailboxNoticeMessageID() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", fmt.Errorf("create mailbox notice identity: %w", err)
	}
	return "<tbmm-" + hex.EncodeToString(value) + "@localhost>", nil
}

func (d *DB) MailboxNoticeRecords(ctx context.Context, migrationID int64) ([]MailboxNoticeRecord, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT id,migration_id,side,enabled,custom_text,subject,message_id,status,uid,uid_validity,size,last_error,delivered_at FROM mailbox_notices WHERE migration_id=? ORDER BY CASE side WHEN 'destination' THEN 0 ELSE 1 END`, migrationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]MailboxNoticeRecord, 0, 2)
	for rows.Next() {
		var record MailboxNoticeRecord
		var delivered sql.NullString
		if err := rows.Scan(&record.ID, &record.MigrationID, &record.Side, &record.Enabled, &record.CustomText, &record.Subject, &record.MessageID, &record.Status, &record.UID, &record.UIDValidity, &record.Size, &record.LastError, &delivered); err != nil {
			return result, err
		}
		if delivered.Valid {
			parsed, _ := time.Parse(time.RFC3339Nano, delivered.String)
			record.DeliveredAt = &parsed
		}
		result = append(result, record)
	}
	return result, rows.Err()
}

func (d *DB) MailboxNoticeStatuses(ctx context.Context, migrationID int64) ([]domain.MailboxNoticeStatus, error) {
	records, err := d.MailboxNoticeRecords(ctx, migrationID)
	if err != nil {
		return nil, err
	}
	result := make([]domain.MailboxNoticeStatus, 0, len(records))
	for _, record := range records {
		result = append(result, domain.MailboxNoticeStatus{
			MailboxNoticeSnapshot: domain.MailboxNoticeSnapshot{Side: record.Side, Enabled: record.Enabled, CustomText: record.CustomText, Subject: record.Subject, MessageID: record.MessageID},
			Status:                record.Status, LastError: record.LastError, DeliveredAt: record.DeliveredAt,
		})
	}
	return result, nil
}

func (d *DB) BeginMailboxNotice(ctx context.Context, id int64) error {
	_, err := d.sql.ExecContext(ctx, `UPDATE mailbox_notices SET status='pending',last_error='' WHERE id=? AND enabled=1 AND status<>'delivered'`, id)
	return err
}

func (d *DB) CompleteMailboxNotice(ctx context.Context, id int64, uidValidity, uid uint32, size int64) error {
	_, err := d.sql.ExecContext(ctx, `UPDATE mailbox_notices SET status='delivered',uid=?,uid_validity=?,size=?,last_error='',delivered_at=? WHERE id=?`, uid, uidValidity, size, time.Now().UTC().Format(time.RFC3339Nano), id)
	return err
}

func (d *DB) FailMailboxNotice(ctx context.Context, id int64, message string) error {
	_, err := d.sql.ExecContext(ctx, `UPDATE mailbox_notices SET status='failed',last_error=? WHERE id=? AND status<>'delivered'`, message, id)
	return err
}

func (d *DB) SourceMailboxNoticeMessageIDs(ctx context.Context, migrationID int64) (map[string]struct{}, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT message_id FROM mailbox_notices WHERE migration_id=? AND side=? AND enabled=1 AND message_id<>''`, migrationID, domain.MailboxNoticeSource)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make(map[string]struct{})
	for rows.Next() {
		var messageID string
		if err := rows.Scan(&messageID); err != nil {
			return nil, err
		}
		result[strings.ToLower(strings.TrimSpace(messageID))] = struct{}{}
	}
	return result, rows.Err()
}
