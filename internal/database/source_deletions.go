package database

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/tenbyte/mail-migrator/internal/domain"
)

type SourceDeletionRecord struct {
	ID                     int64
	MigrationID            int64
	MessageID              int64
	FolderID               int64
	SourceUID              uint32
	DestinationUID         uint32
	DestinationUIDValidity uint32
	SourceSHA              string
	MessageHeaderID        string
	InternalDate           time.Time
	Size                   int64
	DestinationFolder      string
	Status                 string
	Resolution             domain.SourceDeletionResolution
}

func (d *DB) UpsertSourceDeletion(ctx context.Context, migrationID, messageID int64, destinationUID, destinationUIDValidity uint32, subject, sender string) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := d.sql.ExecContext(ctx, `INSERT INTO mail_source_deletions(migration_id,message_id,destination_uid,destination_uidvalidity,subject,sender,detected_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(message_id) DO UPDATE SET
		destination_uid=excluded.destination_uid,destination_uidvalidity=excluded.destination_uidvalidity,
		subject=CASE WHEN excluded.subject<>'' THEN excluded.subject ELSE mail_source_deletions.subject END,
		sender=CASE WHEN excluded.sender<>'' THEN excluded.sender ELSE mail_source_deletions.sender END,
		status=CASE WHEN mail_source_deletions.status IN ('kept','trashed','deleted') THEN mail_source_deletions.status ELSE 'pending' END,
		last_error=CASE WHEN mail_source_deletions.status IN ('kept','trashed','deleted') THEN mail_source_deletions.last_error ELSE '' END,
		updated_at=excluded.updated_at`,
		migrationID, messageID, destinationUID, destinationUIDValidity, subject, sender, now, now)
	return err
}

func (d *DB) ClearSourceDeletion(ctx context.Context, messageID int64) error {
	_, err := d.sql.ExecContext(ctx, `DELETE FROM mail_source_deletions WHERE message_id=?`, messageID)
	return err
}

func (d *DB) ClearUnresolvedSourceDeletion(ctx context.Context, messageID int64) error {
	_, err := d.sql.ExecContext(ctx, `DELETE FROM mail_source_deletions WHERE message_id=? AND status IN ('pending','failed')`, messageID)
	return err
}

func (d *DB) SourceDeletions(ctx context.Context, migrationID int64) ([]domain.SourceDeletion, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT d.id,d.migration_id,f.source_name,f.destination_name,m.source_uid,d.destination_uid,
		d.subject,d.sender,m.internal_date,m.size,d.resolution,d.status,d.last_error,d.detected_at,d.updated_at,d.resolved_at
		FROM mail_source_deletions d JOIN messages m ON m.id=d.message_id JOIN folders f ON f.id=m.folder_id
		WHERE d.migration_id=? AND d.status IN ('pending','failed') ORDER BY f.source_name,m.internal_date,m.source_uid`, migrationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]domain.SourceDeletion, 0)
	for rows.Next() {
		var item domain.SourceDeletion
		var internalDate, detectedAt, updatedAt string
		var resolvedAt sql.NullString
		if err := rows.Scan(&item.ID, &item.MigrationID, &item.Folder, &item.DestinationFolder, &item.SourceUID, &item.DestinationUID,
			&item.Subject, &item.From, &internalDate, &item.Size, &item.Resolution, &item.Status, &item.LastError, &detectedAt, &updatedAt, &resolvedAt); err != nil {
			return nil, err
		}
		item.InternalDate, _ = time.Parse(time.RFC3339Nano, internalDate)
		item.DetectedAt, _ = time.Parse(time.RFC3339Nano, detectedAt)
		item.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updatedAt)
		if resolvedAt.Valid {
			value, parseErr := time.Parse(time.RFC3339Nano, resolvedAt.String)
			if parseErr == nil {
				item.ResolvedAt = &value
			}
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (d *DB) SourceDeletionRecord(ctx context.Context, migrationID, deletionID int64) (SourceDeletionRecord, error) {
	var record SourceDeletionRecord
	var internalDate string
	err := d.sql.QueryRowContext(ctx, `SELECT d.id,d.migration_id,d.message_id,m.folder_id,m.source_uid,d.destination_uid,d.destination_uidvalidity,
		COALESCE(m.sha256,''),COALESCE(m.message_id,''),m.internal_date,m.size,f.destination_name,d.status,d.resolution
		FROM mail_source_deletions d JOIN messages m ON m.id=d.message_id JOIN folders f ON f.id=m.folder_id
		WHERE d.id=? AND d.migration_id=?`, deletionID, migrationID).Scan(
		&record.ID, &record.MigrationID, &record.MessageID, &record.FolderID, &record.SourceUID, &record.DestinationUID,
		&record.DestinationUIDValidity, &record.SourceSHA, &record.MessageHeaderID,
		&internalDate, &record.Size, &record.DestinationFolder, &record.Status, &record.Resolution)
	if err != nil {
		return record, err
	}
	record.InternalDate, _ = time.Parse(time.RFC3339Nano, internalDate)
	return record, nil
}

func (d *DB) ResolveSourceDeletionKeep(ctx context.Context, migrationID, deletionID int64) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	result, err := d.sql.ExecContext(ctx, `UPDATE mail_source_deletions SET resolution=?,status='kept',last_error='',updated_at=?,resolved_at=?
		WHERE id=? AND migration_id=? AND status IN ('pending','failed')`, domain.SourceDeletionKeep, now, now, deletionID, migrationID)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return errors.New("source deletion was already processed or does not exist")
	}
	return nil
}

func (d *DB) CompleteSourceDeletion(ctx context.Context, migrationID, deletionID int64, resolution domain.SourceDeletionResolution) error {
	status := ""
	switch resolution {
	case domain.SourceDeletionTrash:
		status = "trashed"
	case domain.SourceDeletionDelete:
		status = "deleted"
	default:
		return errors.New("invalid deletion resolution")
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	result, err := d.sql.ExecContext(ctx, `UPDATE mail_source_deletions SET resolution=?,status=?,last_error='',updated_at=?,resolved_at=? WHERE id=? AND migration_id=?`,
		resolution, status, now, now, deletionID, migrationID)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (d *DB) FailSourceDeletion(ctx context.Context, migrationID, deletionID int64, resolution domain.SourceDeletionResolution, message string) error {
	result, err := d.sql.ExecContext(ctx, `UPDATE mail_source_deletions SET resolution=?,status='failed',last_error=?,updated_at=?,resolved_at=NULL WHERE id=? AND migration_id=?`, resolution, message, time.Now().UTC().Format(time.RFC3339Nano), deletionID, migrationID)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return sql.ErrNoRows
	}
	return nil
}
