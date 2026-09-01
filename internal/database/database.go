package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/tenbyte/mail-migrator/internal/domain"
)

const schemaVersion = 4

type DB struct{ sql *sql.DB }

func DefaultPath() (string, error) {
	var base string
	var err error
	if runtime.GOOS == "windows" {
		base = os.Getenv("LOCALAPPDATA")
	}
	if base == "" {
		base, err = os.UserConfigDir()
	}
	if err != nil {
		return "", fmt.Errorf("locate application data directory: %w", err)
	}
	if runtime.GOOS == "darwin" {
		base = filepath.Join(base, "Tenbyte Mail Migrator")
	} else {
		base = filepath.Join(base, "Tenbyte", "Mail Migrator")
	}
	if err := os.MkdirAll(base, 0o700); err != nil {
		return "", fmt.Errorf("create application data directory: %w", err)
	}
	return filepath.Join(base, "migrations.db"), nil
}

// RemoveStateFiles deletes the SQLite database and every sidecar or schema
// backup owned by it. Missing files are treated as an already-clean state.
func RemoveStateFiles(path string) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("database path is required")
	}
	paths := []string{path, path + "-wal", path + "-shm"}
	backups, err := filepath.Glob(path + ".v*.bak")
	if err != nil {
		return fmt.Errorf("locate database backups: %w", err)
	}
	paths = append(paths, backups...)
	for _, candidate := range paths {
		if err := os.Remove(candidate); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove %s: %w", filepath.Base(candidate), err)
		}
	}
	return nil
}

func Open(path string) (*DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if err := backupBeforeUpgrade(path); err != nil {
		return nil, fmt.Errorf("backup database before schema upgrade: %w", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, pragma := range []string{"PRAGMA journal_mode=WAL", "PRAGMA foreign_keys=ON", "PRAGMA busy_timeout=5000", "PRAGMA synchronous=FULL"} {
		if _, err := db.ExecContext(ctx, pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("configure sqlite: %w", err)
		}
	}
	store := &DB{sql: db}
	if err := store.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil && !errors.Is(err, os.ErrNotExist) {
		db.Close()
		return nil, err
	}
	return store, nil
}

func (d *DB) Close() error { return d.sql.Close() }

func (d *DB) RecoverInterrupted(ctx context.Context) error {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := recoverMailMessagesTx(ctx, tx, 0); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE migration_services SET status=? WHERE status IN (?,?)`, domain.MigrationInterrupted, domain.MigrationRunning, domain.MigrationPaused); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE migrations SET status=? WHERE status IN (?,?)`, domain.MigrationInterrupted, domain.MigrationRunning, domain.MigrationPaused); err != nil {
		return err
	}
	return tx.Commit()
}

func (d *DB) RecoverMigrationMessages(ctx context.Context, migrationID int64) error {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := recoverMailMessagesTx(ctx, tx, migrationID); err != nil {
		return err
	}
	return tx.Commit()
}

func recoverMailMessagesTx(ctx context.Context, tx *sql.Tx, migrationID int64) error {
	where := `status IN (?,?)`
	args := []any{domain.MessageTransferring, domain.MessageVerifying}
	if migrationID > 0 {
		where += ` AND migration_id=?`
		args = append(args, migrationID)
	}
	query := `UPDATE messages SET
		status=CASE WHEN status=? AND destination_uid IS NOT NULL AND sha256<>'' THEN ? ELSE ? END,
		policy_override=CASE WHEN status=? AND destination_uid IS NOT NULL AND sha256<>'' THEN ? ELSE '' END,
		error_code=CASE WHEN status=? AND destination_uid IS NOT NULL AND sha256<>'' THEN 'TB-MAIL-RECOVERY-VERIFY' ELSE 'TB-MAIL-RECOVERY-APPEND-UNKNOWN' END,
		last_error=CASE WHEN status=? AND destination_uid IS NOT NULL AND sha256<>'' THEN 'After an interruption, only the existing destination message is verified again.' ELSE 'The APPEND outcome is unknown after an interruption and the destination is inventoried before any further action.' END
		WHERE ` + where
	updateArgs := []any{domain.MessageVerifying, domain.MessagePending, domain.MessageUnknown, domain.MessageVerifying, domain.MailIssueVerifyAgain, domain.MessageVerifying, domain.MessageVerifying}
	updateArgs = append(updateArgs, args...)
	if _, err := tx.ExecContext(ctx, query, updateArgs...); err != nil {
		return err
	}
	migrationWhere := ""
	serviceWhere := ""
	countArgs := []any{domain.MessageFailed, domain.MessageQuarantined, domain.MessageUnknown}
	if migrationID > 0 {
		migrationWhere = ` WHERE id=?`
		serviceWhere = ` AND migration_id=?`
		countArgs = append(countArgs, migrationID)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE migrations SET messages_failed=(SELECT COUNT(*) FROM messages WHERE migration_id=migrations.id AND status IN (?,?,?))`+migrationWhere, countArgs...); err != nil {
		return err
	}
	serviceArgs := []any{domain.MessageFailed, domain.MessageQuarantined, domain.MessageUnknown, domain.ServiceMail}
	if migrationID > 0 {
		serviceArgs = append(serviceArgs, migrationID)
	}
	_, err := tx.ExecContext(ctx, `UPDATE migration_services SET items_failed=(SELECT COUNT(*) FROM messages WHERE migration_id=migration_services.migration_id AND status IN (?,?,?)) WHERE kind=?`+serviceWhere, serviceArgs...)
	return err
}

func (d *DB) migrate(ctx context.Context) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS migrations (
			id INTEGER PRIMARY KEY AUTOINCREMENT, created_at TEXT NOT NULL, started_at TEXT, finished_at TEXT,
			status TEXT NOT NULL, source_host TEXT NOT NULL, destination_host TEXT NOT NULL,
			source_username TEXT NOT NULL DEFAULT '', destination_username TEXT NOT NULL DEFAULT '',
			source_port INTEGER NOT NULL DEFAULT 993, destination_port INTEGER NOT NULL DEFAULT 993,
			source_encryption TEXT NOT NULL DEFAULT 'tls', destination_encryption TEXT NOT NULL DEFAULT 'tls',
			source_credential_id TEXT, destination_credential_id TEXT,
			bytes_total INTEGER NOT NULL DEFAULT 0, bytes_copied INTEGER NOT NULL DEFAULT 0,
			messages_total INTEGER NOT NULL DEFAULT 0, messages_copied INTEGER NOT NULL DEFAULT 0,
			messages_failed INTEGER NOT NULL DEFAULT 0, last_error TEXT NOT NULL DEFAULT '')`,
		`CREATE TABLE IF NOT EXISTS folders (
			id INTEGER PRIMARY KEY AUTOINCREMENT, migration_id INTEGER NOT NULL REFERENCES migrations(id) ON DELETE CASCADE,
			source_name TEXT NOT NULL, source_delimiter TEXT NOT NULL DEFAULT '', source_uidvalidity INTEGER NOT NULL DEFAULT 0,
			destination_name TEXT NOT NULL, destination_delimiter TEXT NOT NULL DEFAULT '/', destination_uidvalidity INTEGER NOT NULL DEFAULT 0,
			last_synced_source_uid INTEGER NOT NULL DEFAULT 0, status TEXT NOT NULL DEFAULT 'PENDING', enabled INTEGER NOT NULL DEFAULT 1,
			UNIQUE(migration_id, source_name))`,
		`CREATE TABLE IF NOT EXISTS messages (
			id INTEGER PRIMARY KEY AUTOINCREMENT, migration_id INTEGER NOT NULL REFERENCES migrations(id) ON DELETE CASCADE,
			folder_id INTEGER NOT NULL REFERENCES folders(id) ON DELETE CASCADE, source_uidvalidity INTEGER NOT NULL, source_uid INTEGER NOT NULL,
			destination_uid INTEGER, message_id TEXT, internal_date TEXT, size INTEGER NOT NULL DEFAULT 0, sha256 TEXT,
			status TEXT NOT NULL, attempt_count INTEGER NOT NULL DEFAULT 0, last_error TEXT NOT NULL DEFAULT '', copied_at TEXT,
			error_code TEXT NOT NULL DEFAULT '', destination_sha256 TEXT NOT NULL DEFAULT '', verification_status TEXT NOT NULL DEFAULT '',
			policy_override TEXT NOT NULL DEFAULT '', pre_append_uidnext INTEGER NOT NULL DEFAULT 0, verified_at TEXT,
			UNIQUE(migration_id, folder_id, source_uidvalidity, source_uid))`,
		`CREATE INDEX IF NOT EXISTS messages_pending_idx ON messages(migration_id, status)`,
		`CREATE TABLE IF NOT EXISTS errors (id INTEGER PRIMARY KEY AUTOINCREMENT, migration_id INTEGER NOT NULL REFERENCES migrations(id) ON DELETE CASCADE, folder_id INTEGER, source_uid INTEGER, level TEXT NOT NULL, code TEXT NOT NULL DEFAULT '', message TEXT NOT NULL, created_at TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS settings (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS migration_services (
			id INTEGER PRIMARY KEY AUTOINCREMENT, migration_id INTEGER NOT NULL REFERENCES migrations(id) ON DELETE CASCADE,
			kind TEXT NOT NULL, status TEXT NOT NULL DEFAULT 'READY',
			source_url TEXT NOT NULL DEFAULT '', source_username TEXT NOT NULL DEFAULT '', source_credential_id TEXT,
			destination_url TEXT NOT NULL DEFAULT '', destination_username TEXT NOT NULL DEFAULT '', destination_credential_id TEXT,
			items_total INTEGER NOT NULL DEFAULT 0, items_done INTEGER NOT NULL DEFAULT 0, items_failed INTEGER NOT NULL DEFAULT 0,
			bytes_total INTEGER NOT NULL DEFAULT 0, bytes_done INTEGER NOT NULL DEFAULT 0,
			current_item TEXT NOT NULL DEFAULT '', last_error TEXT NOT NULL DEFAULT '', options_json TEXT NOT NULL DEFAULT '{}',
			UNIQUE(migration_id, kind))`,
		`CREATE TABLE IF NOT EXISTS dav_collections (
			id INTEGER PRIMARY KEY AUTOINCREMENT, migration_id INTEGER NOT NULL REFERENCES migrations(id) ON DELETE CASCADE,
			kind TEXT NOT NULL, source_path TEXT NOT NULL, source_name TEXT NOT NULL DEFAULT '', source_description TEXT NOT NULL DEFAULT '',
			destination_path TEXT NOT NULL, destination_name TEXT NOT NULL DEFAULT '', source_sync_token TEXT NOT NULL DEFAULT '',
			destination_sync_token TEXT NOT NULL DEFAULT '', status TEXT NOT NULL DEFAULT 'PENDING', enabled INTEGER NOT NULL DEFAULT 1,
			UNIQUE(migration_id, kind, source_path))`,
		`CREATE TABLE IF NOT EXISTS dav_resources (
			id INTEGER PRIMARY KEY AUTOINCREMENT, migration_id INTEGER NOT NULL REFERENCES migrations(id) ON DELETE CASCADE,
			collection_id INTEGER NOT NULL REFERENCES dav_collections(id) ON DELETE CASCADE, kind TEXT NOT NULL,
			source_href TEXT NOT NULL, source_uid TEXT NOT NULL DEFAULT '', source_etag TEXT NOT NULL DEFAULT '', source_hash TEXT NOT NULL DEFAULT '',
			destination_href TEXT NOT NULL DEFAULT '', destination_etag TEXT NOT NULL DEFAULT '', size INTEGER NOT NULL DEFAULT 0,
			status TEXT NOT NULL DEFAULT 'PENDING', attempt_count INTEGER NOT NULL DEFAULT 0, last_error TEXT NOT NULL DEFAULT '',
			copied_at TEXT, source_seen INTEGER NOT NULL DEFAULT 1, destination_seen INTEGER NOT NULL DEFAULT 0,
			UNIQUE(migration_id, collection_id, source_href))`,
		`CREATE INDEX IF NOT EXISTS dav_resources_pending_idx ON dav_resources(migration_id,kind,status)`,
		`CREATE TABLE IF NOT EXISTS dav_conflicts (
			id INTEGER PRIMARY KEY AUTOINCREMENT, migration_id INTEGER NOT NULL REFERENCES migrations(id) ON DELETE CASCADE,
			resource_id INTEGER NOT NULL REFERENCES dav_resources(id) ON DELETE CASCADE, kind TEXT NOT NULL,
			source_etag TEXT NOT NULL DEFAULT '', destination_etag TEXT NOT NULL DEFAULT '', resolution TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL, resolved_at TEXT, UNIQUE(resource_id))`,
		`CREATE TABLE IF NOT EXISTS conversion_warnings (
			id INTEGER PRIMARY KEY AUTOINCREMENT, migration_id INTEGER NOT NULL REFERENCES migrations(id) ON DELETE CASCADE,
			resource_id INTEGER REFERENCES dav_resources(id) ON DELETE CASCADE, kind TEXT NOT NULL, code TEXT NOT NULL,
				message TEXT NOT NULL, created_at TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS mail_source_deletions (
				id INTEGER PRIMARY KEY AUTOINCREMENT, migration_id INTEGER NOT NULL REFERENCES migrations(id) ON DELETE CASCADE,
				message_id INTEGER NOT NULL UNIQUE REFERENCES messages(id) ON DELETE CASCADE,
				destination_uid INTEGER NOT NULL, destination_uidvalidity INTEGER NOT NULL DEFAULT 0,
				subject TEXT NOT NULL DEFAULT '', sender TEXT NOT NULL DEFAULT '',
				resolution TEXT NOT NULL DEFAULT '', status TEXT NOT NULL DEFAULT 'pending', last_error TEXT NOT NULL DEFAULT '',
				detected_at TEXT NOT NULL, updated_at TEXT NOT NULL DEFAULT '', resolved_at TEXT)`,
		`CREATE INDEX IF NOT EXISTS mail_source_deletions_pending_idx ON mail_source_deletions(migration_id,status)`,
		`INSERT OR IGNORE INTO migration_services(migration_id,kind,status,source_url,source_username,source_credential_id,destination_url,destination_username,destination_credential_id,items_total,items_done,items_failed,bytes_total,bytes_done,last_error)
			 SELECT id,'mail',status,source_host,source_username,source_credential_id,destination_host,destination_username,destination_credential_id,messages_total,messages_copied,messages_failed,bytes_total,bytes_copied,last_error FROM migrations`,
		`UPDATE migration_services SET bytes_total=MAX(bytes_done,COALESCE((SELECT SUM(size) FROM messages WHERE messages.migration_id=migration_services.migration_id),0)) WHERE kind='mail' AND bytes_total=0 AND items_total>0`,
		`UPDATE migrations SET bytes_total=MAX(bytes_copied,COALESCE((SELECT SUM(bytes_total) FROM migration_services WHERE migration_services.migration_id=migrations.id),0)) WHERE bytes_total=0 AND messages_total>0`,
	}
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, statement := range statements {
		if _, execErr := tx.ExecContext(ctx, statement); execErr != nil {
			return fmt.Errorf("apply database schema: %w", execErr)
		}
	}
	columns := []struct{ table, name, definition string }{
		{"messages", "error_code", "TEXT NOT NULL DEFAULT ''"},
		{"messages", "destination_sha256", "TEXT NOT NULL DEFAULT ''"},
		{"messages", "verification_status", "TEXT NOT NULL DEFAULT ''"},
		{"messages", "policy_override", "TEXT NOT NULL DEFAULT ''"},
		{"messages", "pre_append_uidnext", "INTEGER NOT NULL DEFAULT 0"},
		{"messages", "verified_at", "TEXT"},
		{"errors", "code", "TEXT NOT NULL DEFAULT ''"},
		{"mail_source_deletions", "updated_at", "TEXT NOT NULL DEFAULT ''"},
	}
	for _, column := range columns {
		exists, err := columnExists(ctx, tx, column.table, column.name)
		if err != nil {
			return err
		}
		if !exists {
			if _, err := tx.ExecContext(ctx, fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", column.table, column.name, column.definition)); err != nil {
				return fmt.Errorf("add database column %s.%s: %w", column.table, column.name, err)
			}
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE mail_source_deletions SET updated_at=detected_at WHERE updated_at=''`); err != nil {
		return fmt.Errorf("backfill source deletion timestamps: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO schema_migrations(version, applied_at) VALUES(?, ?)`, schemaVersion, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return err
	}
	return tx.Commit()
}

func columnExists(ctx context.Context, tx *sql.Tx, table, wanted string) (bool, error) {
	rows, err := tx.QueryContext(ctx, "PRAGMA table_info("+table+")")
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, dataType string
		var notNull, primaryKey int
		var defaultValue sql.NullString
		if err := rows.Scan(&cid, &name, &dataType, &notNull, &defaultValue, &primaryKey); err != nil {
			return false, err
		}
		if name == wanted {
			return true, nil
		}
	}
	return false, rows.Err()
}

func backupBeforeUpgrade(path string) error {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) || (err == nil && info.Size() == 0) {
		return nil
	}
	if err != nil {
		return err
	}
	probe, err := sql.Open("sqlite", path)
	if err != nil {
		return err
	}
	var version int
	queryErr := probe.QueryRow(`SELECT COALESCE(MAX(version),0) FROM schema_migrations`).Scan(&version)
	_ = probe.Close()
	if queryErr != nil || version == 0 || version >= schemaVersion {
		return nil
	}
	backupPath := fmt.Sprintf("%s.v%d.bak", path, version)
	if _, err := os.Stat(backupPath); err == nil {
		return nil
	}
	source, err := os.Open(path)
	if err != nil {
		return err
	}
	defer source.Close()
	destination, err := os.OpenFile(backupPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(destination, source)
	closeErr := destination.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

func (d *DB) CreateMigration(ctx context.Context, request domain.StartRequest) (int64, error) {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	totalMessages, totalBytes := int64(0), int64(0)
	for _, mapping := range request.Mappings {
		if mapping.Enabled {
			totalMessages += int64(mapping.Source.Messages)
			totalBytes += mapping.Source.Size
		}
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO migrations(created_at,status,source_host,destination_host,source_username,destination_username,source_port,destination_port,source_encryption,destination_encryption,source_credential_id,destination_credential_id,messages_total,bytes_total) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		now, domain.MigrationReady, request.Source.Host, request.Destination.Host, request.Source.Username, request.Destination.Username, request.Source.Port, request.Destination.Port, request.Source.Encryption, request.Destination.Encryption, nullable(request.Source.CredentialID), nullable(request.Destination.CredentialID), totalMessages, totalBytes)
	if err != nil {
		return 0, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, err
	}
	for _, mapping := range request.Mappings {
		_, err = tx.ExecContext(ctx, `INSERT INTO folders(migration_id,source_name,source_delimiter,source_uidvalidity,destination_name,destination_delimiter,status,enabled) VALUES(?,?,?,?,?,?,?,?)`, id, mapping.Source.Name, string(mapping.Source.Delimiter), mapping.Source.UIDValidity, mapping.DestinationName, string(mapping.DestinationDelimiter), domain.MessagePending, mapping.Enabled)
		if err != nil {
			return 0, err
		}
	}
	optionsJSON, _ := json.Marshal(request.Options)
	_, err = tx.ExecContext(ctx, `INSERT INTO migration_services(migration_id,kind,status,source_url,source_username,source_credential_id,destination_url,destination_username,destination_credential_id,items_total,bytes_total,options_json) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		id, domain.ServiceMail, domain.MigrationReady, request.Source.Host, request.Source.Username, nullable(request.Source.CredentialID), request.Destination.Host, request.Destination.Username, nullable(request.Destination.CredentialID), totalMessages, totalBytes, string(optionsJSON))
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return id, nil
}

type FolderRecord struct {
	ID                                                       int64
	SourceName, DestinationName                              string
	SourceDelimiter, DestinationDelimiter                    rune
	SourceUIDValidity, DestinationUIDValidity, LastSyncedUID uint32
	Enabled                                                  bool
}

type CopiedMessageRecord struct {
	ID                int64
	SourceUIDValidity uint32
	SourceUID         uint32
	DestinationUID    uint32
	MessageID         string
	SourceSHA         string
	InternalDate      time.Time
	Size              int64
}

type MessageTransferRecord struct {
	ID               int64
	DestinationUID   uint32
	SourceSHA        string
	DestinationSHA   string
	Verification     string
	PolicyOverride   string
	PreAppendUIDNext uint32
	State            domain.MessageState
}

func (d *DB) MessageTransfer(ctx context.Context, migrationID, folderID int64, uidValidity, uid uint32) (MessageTransferRecord, error) {
	var record MessageTransferRecord
	var destinationUID sql.NullInt64
	err := d.sql.QueryRowContext(ctx, `SELECT id,destination_uid,COALESCE(sha256,''),destination_sha256,verification_status,policy_override,pre_append_uidnext,status FROM messages WHERE migration_id=? AND folder_id=? AND source_uidvalidity=? AND source_uid=?`, migrationID, folderID, uidValidity, uid).Scan(&record.ID, &destinationUID, &record.SourceSHA, &record.DestinationSHA, &record.Verification, &record.PolicyOverride, &record.PreAppendUIDNext, &record.State)
	if destinationUID.Valid {
		record.DestinationUID = uint32(destinationUID.Int64)
	}
	return record, err
}

func (d *DB) LoadRequest(ctx context.Context, migrationID int64) (domain.StartRequest, error) {
	var request domain.StartRequest
	var sourceCredential, destinationCredential sql.NullString
	err := d.sql.QueryRowContext(ctx, `SELECT source_host,destination_host,source_username,destination_username,source_port,destination_port,source_encryption,destination_encryption,source_credential_id,destination_credential_id FROM migrations WHERE id=?`, migrationID).Scan(
		&request.Source.Host, &request.Destination.Host, &request.Source.Username, &request.Destination.Username, &request.Source.Port, &request.Destination.Port, &request.Source.Encryption, &request.Destination.Encryption, &sourceCredential, &destinationCredential)
	if err != nil {
		return request, err
	}
	request.Source.CredentialID, request.Destination.CredentialID = sourceCredential.String, destinationCredential.String
	request.Source.RememberCredential = request.Source.CredentialID != ""
	request.Destination.RememberCredential = request.Destination.CredentialID != ""
	request.MigrationID = migrationID
	request.Options = domain.DefaultTransferOptions()
	var optionsJSON string
	if err := d.sql.QueryRowContext(ctx, `SELECT options_json FROM migration_services WHERE migration_id=? AND kind=?`, migrationID, domain.ServiceMail).Scan(&optionsJSON); err == nil && optionsJSON != "" {
		_ = json.Unmarshal([]byte(optionsJSON), &request.Options)
		var fields map[string]json.RawMessage
		_ = json.Unmarshal([]byte(optionsJSON), &fields)
		if _, hasMode := fields["verificationMode"]; !hasMode {
			if _, hasLegacyVerify := fields["verifyAfter"]; hasLegacyVerify && !request.Options.VerifyAfter {
				request.Options.VerificationMode = domain.VerificationMetadata
			}
		}
	}
	folders, err := d.Folders(ctx, migrationID)
	if err != nil {
		return request, err
	}
	for _, folder := range folders {
		request.Mappings = append(request.Mappings, domain.FolderMapping{Source: domain.Mailbox{Name: folder.SourceName, Delimiter: folder.SourceDelimiter, UIDValidity: folder.SourceUIDValidity, Selectable: folder.Enabled}, DestinationName: folder.DestinationName, DestinationDelimiter: folder.DestinationDelimiter, Enabled: folder.Enabled})
	}
	return request, nil
}

func (d *DB) UpdateCredentialIDs(ctx context.Context, migrationID int64, sourceID, destinationID string) error {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE migrations SET source_credential_id=?,destination_credential_id=? WHERE id=?`, nullable(sourceID), nullable(destinationID), migrationID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE migration_services SET source_credential_id=?,destination_credential_id=? WHERE migration_id=? AND kind=?`, nullable(sourceID), nullable(destinationID), migrationID, domain.ServiceMail); err != nil {
		return err
	}
	return tx.Commit()
}

func (d *DB) RefreshMigrationScope(ctx context.Context, migrationID int64, request domain.StartRequest) error {
	var messages, bytes int64
	for _, mapping := range request.Mappings {
		if mapping.Enabled {
			messages += int64(mapping.Source.Messages)
			bytes += mapping.Source.Size
		}
	}
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE migration_services SET items_total=MAX(?,items_done),bytes_total=MAX(?,bytes_done) WHERE migration_id=? AND kind=?`, messages, bytes, migrationID, domain.ServiceMail); err != nil {
		return err
	}
	var totalItems, totalBytes int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(items_total),0),COALESCE(SUM(bytes_total),0) FROM migration_services WHERE migration_id=?`, migrationID).Scan(&totalItems, &totalBytes); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE migrations SET messages_total=MAX(?,messages_copied),bytes_total=MAX(?,bytes_copied),finished_at=NULL,last_error='' WHERE id=?`, totalItems, totalBytes, migrationID); err != nil {
		return err
	}
	return tx.Commit()
}

func (d *DB) Folders(ctx context.Context, migrationID int64) ([]FolderRecord, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT id,source_name,source_delimiter,source_uidvalidity,destination_name,destination_delimiter,destination_uidvalidity,last_synced_source_uid,enabled FROM folders WHERE migration_id=? ORDER BY length(destination_name),destination_name`, migrationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []FolderRecord
	for rows.Next() {
		var f FolderRecord
		var delimiter, destinationDelimiter string
		if err := rows.Scan(&f.ID, &f.SourceName, &delimiter, &f.SourceUIDValidity, &f.DestinationName, &destinationDelimiter, &f.DestinationUIDValidity, &f.LastSyncedUID, &f.Enabled); err != nil {
			return nil, err
		}
		if delimiter != "" {
			f.SourceDelimiter, _ = firstRune(delimiter)
		}
		if destinationDelimiter != "" {
			f.DestinationDelimiter, _ = firstRune(destinationDelimiter)
		}
		result = append(result, f)
	}
	return result, rows.Err()
}

func (d *DB) MarkRunning(ctx context.Context, id int64) error {
	if _, err := d.sql.ExecContext(ctx, `UPDATE migrations SET started_at=COALESCE(started_at,?) WHERE id=?`, time.Now().UTC().Format(time.RFC3339Nano), id); err != nil {
		return err
	}
	return d.MarkService(ctx, id, domain.ServiceMail, domain.MigrationRunning, "")
}

func (d *DB) MarkMigration(ctx context.Context, id int64, state domain.MigrationState, message string) error {
	return d.MarkService(ctx, id, domain.ServiceMail, state, message)
}

func (d *DB) BeginMessage(ctx context.Context, migrationID, folderID int64, uidValidity, uid uint32, size int64, messageID string, date time.Time) (bool, error) {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var status, policy string
	err = tx.QueryRowContext(ctx, `SELECT status,policy_override FROM messages WHERE migration_id=? AND folder_id=? AND source_uidvalidity=? AND source_uid=?`, migrationID, folderID, uidValidity, uid).Scan(&status, &policy)
	if err == nil {
		if status == string(domain.MessageCopied) || status == string(domain.MessageSkipped) || status == string(domain.MessageUnknown) || (status == string(domain.MessageQuarantined) && policy == "") || (status == string(domain.MessageFailed) && policy == "") {
			return true, nil
		}
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO messages(migration_id,folder_id,source_uidvalidity,source_uid,message_id,internal_date,size,status,attempt_count) VALUES(?,?,?,?,?,?,?,?,1) ON CONFLICT(migration_id,folder_id,source_uidvalidity,source_uid) DO UPDATE SET status=excluded.status,message_id=excluded.message_id,internal_date=excluded.internal_date,size=excluded.size,attempt_count=messages.attempt_count+1,last_error='',error_code=''`, migrationID, folderID, uidValidity, uid, nullable(messageID), date.UTC().Format(time.RFC3339Nano), size, domain.MessageTransferring)
	if err != nil {
		return false, err
	}
	return false, tx.Commit()
}

func (d *DB) CompleteMessage(ctx context.Context, migrationID, folderID int64, uidValidity, sourceUID, destinationUID uint32, size int64, sha string) error {
	return d.CompleteVerifiedMessage(ctx, migrationID, folderID, uidValidity, sourceUID, destinationUID, size, sha, "", "metadata")
}

func (d *DB) CompleteVerifiedMessage(ctx context.Context, migrationID, folderID int64, uidValidity, sourceUID, destinationUID uint32, size int64, sourceSHA, destinationSHA, verification string) error {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	result, err := tx.ExecContext(ctx, `UPDATE messages SET status=?,destination_uid=?,size=?,sha256=?,destination_sha256=?,verification_status=?,copied_at=?,verified_at=?,last_error='',error_code='',policy_override='' WHERE migration_id=? AND folder_id=? AND source_uidvalidity=? AND source_uid=?`, domain.MessageCopied, nullableUint(destinationUID), size, sourceSHA, destinationSHA, verification, now, now, migrationID, folderID, uidValidity, sourceUID)
	if err != nil {
		return err
	}
	affected, _ := result.RowsAffected()
	if affected == 0 {
		return errors.New("message state disappeared during transfer")
	}
	_, err = tx.ExecContext(ctx, `UPDATE folders SET last_synced_source_uid=MAX(last_synced_source_uid,?),status='RUNNING' WHERE id=?`, sourceUID, folderID)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE migrations SET messages_copied=messages_copied+1,bytes_copied=bytes_copied+?,messages_failed=(SELECT COUNT(*) FROM messages WHERE migration_id=? AND status IN (?,?,?)) WHERE id=?`, size, migrationID, domain.MessageFailed, domain.MessageQuarantined, domain.MessageUnknown, migrationID)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE migration_services SET items_done=items_done+1,bytes_done=bytes_done+? WHERE migration_id=? AND kind=?`, size, migrationID, domain.ServiceMail); err != nil {
		return err
	}
	return tx.Commit()
}

func (d *DB) FailMessage(ctx context.Context, migrationID, folderID int64, uid uint32, state domain.MessageState, message string) error {
	return d.FailMessageCode(ctx, migrationID, folderID, uid, state, "", message)
}

func (d *DB) FailMessageCode(ctx context.Context, migrationID, folderID int64, uid uint32, state domain.MessageState, code, message string) error {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	verificationFailed := strings.HasPrefix(code, "TB-MAIL-VERIFY-")
	_, err = tx.ExecContext(ctx, `UPDATE messages SET status=?,error_code=?,last_error=?,verification_status=CASE WHEN ? THEN 'failed' ELSE verification_status END WHERE id=(SELECT id FROM messages WHERE migration_id=? AND folder_id=? AND source_uid=? ORDER BY id DESC LIMIT 1)`, state, code, message, verificationFailed, migrationID, folderID, uid)
	if err != nil {
		return err
	}
	level := "ERROR"
	if state == domain.MessageRetryPending {
		level = "WARN"
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO errors(migration_id,folder_id,source_uid,level,code,message,created_at) VALUES(?,?,?,?,?,?,?)`, migrationID, folderID, uid, level, code, message, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return err
	}
	if state == domain.MessageFailed || state == domain.MessageQuarantined || state == domain.MessageUnknown {
		_, err = tx.ExecContext(ctx, `UPDATE migrations SET messages_failed=(SELECT COUNT(*) FROM messages WHERE migration_id=? AND status IN (?,?,?)),last_error=? WHERE id=?`, migrationID, domain.MessageFailed, domain.MessageQuarantined, domain.MessageUnknown, message, migrationID)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE migration_services SET items_failed=(SELECT COUNT(*) FROM messages WHERE migration_id=? AND status IN (?,?,?)),last_error=? WHERE migration_id=? AND kind=?`, migrationID, domain.MessageFailed, domain.MessageQuarantined, domain.MessageUnknown, message, migrationID, domain.ServiceMail); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (d *DB) RecordMessageIssue(ctx context.Context, migrationID, folderID int64, uidValidity, uid uint32, size int64, messageID string, date time.Time, state domain.MessageState, code, message string) error {
	skipped, err := d.BeginMessage(ctx, migrationID, folderID, uidValidity, uid, size, messageID, date)
	if err != nil {
		return err
	}
	if skipped {
		return nil
	}
	return d.FailMessageCode(ctx, migrationID, folderID, uid, state, code, message)
}

func (d *DB) SetMessageTransferIdentity(ctx context.Context, migrationID, folderID int64, uid uint32, sourceSHA string, preAppendUIDNext uint32) error {
	_, err := d.sql.ExecContext(ctx, `UPDATE messages SET sha256=?,pre_append_uidnext=?,status=?,verification_status='pending' WHERE id=(SELECT id FROM messages WHERE migration_id=? AND folder_id=? AND source_uid=? ORDER BY id DESC LIMIT 1)`, sourceSHA, preAppendUIDNext, domain.MessageTransferring, migrationID, folderID, uid)
	return err
}

func (d *DB) MarkMessageVerifying(ctx context.Context, migrationID, folderID int64, uid, destinationUID uint32, sourceSHA string) error {
	_, err := d.sql.ExecContext(ctx, `UPDATE messages SET status=?,destination_uid=?,sha256=?,verification_status='pending' WHERE id=(SELECT id FROM messages WHERE migration_id=? AND folder_id=? AND source_uid=? ORDER BY id DESC LIMIT 1)`, domain.MessageVerifying, nullableUint(destinationUID), sourceSHA, migrationID, folderID, uid)
	return err
}

func (d *DB) AddMessageWarning(ctx context.Context, migrationID, folderID int64, uid uint32, code, message string) error {
	_, err := d.sql.ExecContext(ctx, `INSERT INTO errors(migration_id,folder_id,source_uid,level,code,message,created_at) VALUES(?,?,?,?,?,?,?)`, migrationID, folderID, uid, "WARN", code, message, time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

// AddServiceError persists failures that happen before a concrete source
// message can be opened. Without a message row these errors would otherwise be
// visible only in the transient progress event and absent from the report.
func (d *DB) AddServiceError(ctx context.Context, migrationID, folderID int64, kind domain.ServiceKind, code, message string) error {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO errors(migration_id,folder_id,source_uid,level,code,message,created_at) VALUES(?,?,NULL,'ERROR',?,?,?)`, migrationID, nullableInt64(folderID), code, message, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE migrations SET last_error=? WHERE id=?`, message, migrationID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE migration_services SET last_error=? WHERE migration_id=? AND kind=?`, message, migrationID, kind); err != nil {
		return err
	}
	return tx.Commit()
}

func (d *DB) SetFolderUIDValidity(ctx context.Context, folderID int64, uidValidity uint32, reset bool) error {
	if reset {
		_, err := d.sql.ExecContext(ctx, `UPDATE folders SET source_uidvalidity=?,last_synced_source_uid=0,status='RECONCILE' WHERE id=?`, uidValidity, folderID)
		return err
	}
	_, err := d.sql.ExecContext(ctx, `UPDATE folders SET source_uidvalidity=? WHERE id=?`, uidValidity, folderID)
	return err
}

func (d *DB) SetDestinationUIDValidity(ctx context.Context, folderID int64, uidValidity uint32) error {
	_, err := d.sql.ExecContext(ctx, `UPDATE folders SET destination_uidvalidity=? WHERE id=?`, uidValidity, folderID)
	return err
}

func (d *DB) UnfinishedUIDs(ctx context.Context, migrationID, folderID int64, uidValidity uint32) ([]uint32, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT source_uid FROM messages WHERE migration_id=? AND folder_id=? AND source_uidvalidity=? AND (status IN (?,?,?,?) OR policy_override<>'') ORDER BY source_uid`, migrationID, folderID, uidValidity, domain.MessagePending, domain.MessageRetryPending, domain.MessageTransferring, domain.MessageUnknown)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []uint32
	for rows.Next() {
		var uid uint32
		if err := rows.Scan(&uid); err != nil {
			return nil, err
		}
		result = append(result, uid)
	}
	return result, rows.Err()
}

func (d *DB) CopiedMessages(ctx context.Context, migrationID, folderID int64, sourceUIDValidity uint32) ([]CopiedMessageRecord, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT id,source_uidvalidity,source_uid,COALESCE(destination_uid,0),COALESCE(message_id,''),COALESCE(sha256,''),internal_date,size FROM messages WHERE migration_id=? AND folder_id=? AND source_uidvalidity=? AND status=? ORDER BY source_uid`, migrationID, folderID, sourceUIDValidity, domain.MessageCopied)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]CopiedMessageRecord, 0)
	for rows.Next() {
		var record CopiedMessageRecord
		var internalDate sql.NullString
		if err := rows.Scan(&record.ID, &record.SourceUIDValidity, &record.SourceUID, &record.DestinationUID, &record.MessageID, &record.SourceSHA, &internalDate, &record.Size); err != nil {
			return nil, err
		}
		if internalDate.Valid {
			record.InternalDate, _ = time.Parse(time.RFC3339Nano, internalDate.String)
		}
		result = append(result, record)
	}
	return result, rows.Err()
}

func (d *DB) ConfirmDestination(ctx context.Context, messageID int64, destinationUID uint32) error {
	_, err := d.sql.ExecContext(ctx, `UPDATE messages SET destination_uid=?,last_error='' WHERE id=?`, nullableUint(destinationUID), messageID)
	return err
}

// RequeueCopiedMessage makes a previously completed source message eligible
// for another APPEND after reconciliation proves that its destination copy is
// gone. Counters are rolled back in the same transaction so progress remains
// truthful while the replacement is transferred.
func (d *DB) RequeueCopiedMessage(ctx context.Context, migrationID, messageID int64) (bool, int64, error) {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return false, 0, err
	}
	defer tx.Rollback()
	var status string
	var size int64
	if err := tx.QueryRowContext(ctx, `SELECT status,size FROM messages WHERE id=? AND migration_id=?`, messageID, migrationID).Scan(&status, &size); err != nil {
		return false, 0, err
	}
	if status != string(domain.MessageCopied) {
		return false, size, nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE messages SET status=?,destination_uid=NULL,last_error='destination copy missing' WHERE id=?`, domain.MessagePending, messageID); err != nil {
		return false, 0, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE migrations SET messages_copied=MAX(0,messages_copied-1),bytes_copied=MAX(0,bytes_copied-?) WHERE id=?`, size, migrationID); err != nil {
		return false, 0, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE migration_services SET items_done=MAX(0,items_done-1),bytes_done=MAX(0,bytes_done-?) WHERE migration_id=? AND kind=?`, size, migrationID, domain.ServiceMail); err != nil {
		return false, 0, err
	}
	if err := tx.Commit(); err != nil {
		return false, 0, err
	}
	return true, size, nil
}

func (d *DB) Recent(ctx context.Context, limit int) ([]domain.RecentMigration, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT id,created_at,finished_at,status,source_host,destination_host,source_username,destination_username,messages_total,messages_copied,messages_failed,bytes_total,bytes_copied FROM migrations ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]domain.RecentMigration, 0)
	for rows.Next() {
		var m domain.RecentMigration
		var created string
		var finished sql.NullString
		if err := rows.Scan(&m.ID, &created, &finished, &m.State, &m.SourceHost, &m.DestinationHost, &m.SourceUsername, &m.DestinationUsername, &m.MessagesTotal, &m.MessagesCopied, &m.MessagesFailed, &m.BytesTotal, &m.BytesCopied); err != nil {
			return nil, err
		}
		m.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		if finished.Valid {
			f, _ := time.Parse(time.RFC3339Nano, finished.String)
			m.FinishedAt = &f
		}
		result = append(result, m)
	}
	if err := rows.Err(); err != nil {
		return result, err
	}
	_ = rows.Close()
	for i := range result {
		result[i].Services, _ = d.JobServices(ctx, result[i].ID)
	}
	return result, nil
}

func (d *DB) RecentByID(ctx context.Context, id int64) (domain.RecentMigration, error) {
	items, err := d.recentWhere(ctx, id)
	if err != nil || len(items) == 0 {
		if err == nil {
			err = sql.ErrNoRows
		}
		return domain.RecentMigration{}, err
	}
	return items[0], nil
}

func (d *DB) Report(ctx context.Context, id int64) (domain.Report, error) {
	migration, err := d.RecentByID(ctx, id)
	if err != nil {
		return domain.Report{}, err
	}
	report := domain.Report{Migration: migration}
	report.Services, _ = d.ServiceProgresses(ctx, id)
	if err := d.sql.QueryRowContext(ctx, `SELECT COUNT(*) FROM folders WHERE migration_id=? AND enabled=1`, id).Scan(&report.Folders); err != nil {
		return report, err
	}
	if err := d.sql.QueryRowContext(ctx, `SELECT COUNT(*) FROM errors WHERE migration_id=? AND level='WARN'`, id).Scan(&report.Warnings); err != nil {
		return report, err
	}
	if err := d.sql.QueryRowContext(ctx, `SELECT COUNT(*) FROM errors WHERE migration_id=? AND level='ERROR'`, id).Scan(&report.Errors); err != nil {
		return report, err
	}
	report.WarningDetails, err = d.reportEvents(ctx, id, "WARN")
	if err != nil {
		return report, err
	}
	report.ErrorDetails, err = d.reportEvents(ctx, id, "ERROR")
	if err != nil {
		return report, err
	}
	_ = d.sql.QueryRowContext(ctx, `SELECT COUNT(*) FROM conversion_warnings WHERE migration_id=? AND code='UPDATED'`, id).Scan(&report.Updated)
	_ = d.sql.QueryRowContext(ctx, `SELECT COUNT(*) FROM conversion_warnings WHERE migration_id=? AND code NOT IN ('REPAIRED','UPDATED')`, id).Scan(&report.Converted)
	_ = d.sql.QueryRowContext(ctx, `SELECT COUNT(*) FROM conversion_warnings WHERE migration_id=? AND code='REPAIRED'`, id).Scan(&report.Repaired)
	_ = d.sql.QueryRowContext(ctx, `SELECT COUNT(*) FROM dav_resources WHERE migration_id=? AND status=?`, id, domain.MessageSkipped).Scan(&report.Skipped)
	var skippedMail int64
	_ = d.sql.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE migration_id=? AND status=?`, id, domain.MessageSkipped).Scan(&skippedMail)
	report.Skipped += skippedMail
	_ = d.sql.QueryRowContext(ctx, `SELECT COUNT(*) FROM dav_conflicts WHERE migration_id=?`, id).Scan(&report.Conflicts)
	_ = d.sql.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE migration_id=? AND verification_status='verified'`, id).Scan(&report.Verified)
	_ = d.sql.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE migration_id=? AND status=?`, id, domain.MessageQuarantined).Scan(&report.Quarantined)
	_ = d.sql.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE migration_id=? AND status=?`, id, domain.MessageUnknown).Scan(&report.Unknown)
	_ = d.sql.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE migration_id=? AND verification_status='failed'`, id).Scan(&report.VerificationFailed)
	_ = d.sql.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE migration_id=? AND verification_status='deduplicated'`, id).Scan(&report.Deduplicated)
	_ = d.sql.QueryRowContext(ctx, `SELECT COUNT(*) FROM mail_source_deletions WHERE migration_id=? AND status='kept'`, id).Scan(&report.SourceDeletionsKept)
	_ = d.sql.QueryRowContext(ctx, `SELECT COUNT(*) FROM mail_source_deletions WHERE migration_id=? AND status='trashed'`, id).Scan(&report.SourceDeletionsTrashed)
	_ = d.sql.QueryRowContext(ctx, `SELECT COUNT(*) FROM mail_source_deletions WHERE migration_id=? AND status='deleted'`, id).Scan(&report.SourceDeletionsDeleted)
	_ = d.sql.QueryRowContext(ctx, `SELECT COUNT(*) FROM mail_source_deletions WHERE migration_id=? AND status='failed'`, id).Scan(&report.SourceDeletionErrors)
	report.MailIssues, _ = d.MailIssues(ctx, id)
	if migration.MessagesFailed == 0 && migration.MessagesCopied == migration.MessagesTotal && report.VerificationFailed == 0 && report.Unknown == 0 && report.Quarantined == 0 {
		report.Verification = "All tracked source objects were transferred successfully and verified using the selected mode."
	} else {
		report.Verification = "Verification is incomplete. Review failed, skipped, or conflicting objects."
	}
	return report, nil
}

func (d *DB) reportEvents(ctx context.Context, migrationID int64, level string) ([]domain.ReportEvent, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT COALESCE(f.source_name,''),COALESCE(e.source_uid,0),e.level,e.code,e.message,e.created_at FROM errors e LEFT JOIN folders f ON f.id=e.folder_id WHERE e.migration_id=? AND e.level=? ORDER BY e.id`, migrationID, level)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	events := make([]domain.ReportEvent, 0)
	for rows.Next() {
		var event domain.ReportEvent
		var createdAt string
		if err := rows.Scan(&event.Folder, &event.SourceUID, &event.Level, &event.Code, &event.Message, &createdAt); err != nil {
			return events, err
		}
		event.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdAt)
		events = append(events, event)
	}
	return events, rows.Err()
}

func (d *DB) MailIssues(ctx context.Context, migrationID int64) ([]domain.MailIssue, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT m.id,m.migration_id,f.source_name,m.source_uid,m.size,m.status,m.error_code,m.last_error,m.verification_status,m.destination_uid,COALESCE(m.sha256,'') FROM messages m JOIN folders f ON f.id=m.folder_id WHERE m.migration_id=? AND (m.error_code<>'' OR m.status IN (?,?,?,?)) ORDER BY f.source_name,m.source_uid`, migrationID, domain.MessageQuarantined, domain.MessageUnknown, domain.MessageFailed, domain.MessageSkipped)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	issues := make([]domain.MailIssue, 0)
	for rows.Next() {
		var issue domain.MailIssue
		var destinationUID sql.NullInt64
		var sourceSHA string
		if err := rows.Scan(&issue.ID, &issue.MigrationID, &issue.Folder, &issue.SourceUID, &issue.Size, &issue.State, &issue.ErrorCode, &issue.Message, &issue.Verification, &destinationUID, &sourceSHA); err != nil {
			return nil, err
		}
		switch issue.State {
		case domain.MessageQuarantined:
			issue.AllowedActions = []domain.MailIssueResolution{domain.MailIssueRetry, domain.MailIssueKeepSkipped}
			if issue.ErrorCode == "TB-MAIL-SOURCE-EMPTY" {
				issue.AllowedActions = []domain.MailIssueResolution{domain.MailIssueTransferAnyway, domain.MailIssueKeepSkipped}
			}
		case domain.MessageUnknown:
			issue.AllowedActions = []domain.MailIssueResolution{domain.MailIssueKeepSkipped}
			if destinationUID.Valid && destinationUID.Int64 > 0 && sourceSHA != "" {
				issue.AllowedActions = []domain.MailIssueResolution{domain.MailIssueVerifyAgain, domain.MailIssueKeepSkipped}
			}
		case domain.MessageFailed:
			if issue.Verification == "failed" {
				issue.AllowedActions = []domain.MailIssueResolution{domain.MailIssueVerifyAgain, domain.MailIssueKeepSkipped}
			} else {
				issue.AllowedActions = []domain.MailIssueResolution{domain.MailIssueRetry, domain.MailIssueKeepSkipped}
			}
		}
		issues = append(issues, issue)
	}
	return issues, rows.Err()
}

func (d *DB) ResolveMailIssue(ctx context.Context, messageID int64, resolution domain.MailIssueResolution) error {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var migrationID int64
	var state, code, sourceSHA string
	var destinationUID sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT migration_id,status,error_code,COALESCE(sha256,''),destination_uid FROM messages WHERE id=?`, messageID).Scan(&migrationID, &state, &code, &sourceSHA, &destinationUID); err != nil {
		return err
	}
	nextState := domain.MessagePending
	policy := string(resolution)
	switch resolution {
	case domain.MailIssueTransferAnyway:
		if state != string(domain.MessageQuarantined) || code != "TB-MAIL-SOURCE-EMPTY" {
			return errors.New("only a quarantined empty message can be approved explicitly")
		}
	case domain.MailIssueRetry:
		if state == string(domain.MessageUnknown) {
			return errors.New("an unknown APPEND outcome cannot be retried without verification")
		}
	case domain.MailIssueVerifyAgain:
		if !destinationUID.Valid || destinationUID.Int64 <= 0 || sourceSHA == "" {
			return errors.New("destination UID or source hash is missing for verification")
		}
	case domain.MailIssueKeepSkipped:
		nextState = domain.MessageSkipped
		policy = ""
	default:
		return errors.New("invalid issue resolution")
	}
	if _, err := tx.ExecContext(ctx, `UPDATE messages SET status=?,policy_override=?,error_code=CASE WHEN ?=? THEN error_code ELSE '' END,last_error=CASE WHEN ?=? THEN last_error ELSE '' END,verification_status=CASE WHEN ?=? THEN 'pending' ELSE verification_status END WHERE id=?`, nextState, policy, nextState, domain.MessageSkipped, nextState, domain.MessageSkipped, resolution, domain.MailIssueVerifyAgain, messageID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE migrations SET messages_failed=(SELECT COUNT(*) FROM messages WHERE migration_id=? AND status IN (?,?,?)) WHERE id=?`, migrationID, domain.MessageFailed, domain.MessageQuarantined, domain.MessageUnknown, migrationID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE migration_services SET items_failed=(SELECT COUNT(*) FROM messages WHERE migration_id=? AND status IN (?,?,?)) WHERE migration_id=? AND kind=?`, migrationID, domain.MessageFailed, domain.MessageQuarantined, domain.MessageUnknown, migrationID, domain.ServiceMail); err != nil {
		return err
	}
	return tx.Commit()
}

func (d *DB) recentWhere(ctx context.Context, id int64) ([]domain.RecentMigration, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT id,created_at,finished_at,status,source_host,destination_host,source_username,destination_username,messages_total,messages_copied,messages_failed,bytes_total,bytes_copied FROM migrations WHERE id=?`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.RecentMigration
	for rows.Next() {
		var m domain.RecentMigration
		var c string
		var f sql.NullString
		if err := rows.Scan(&m.ID, &c, &f, &m.State, &m.SourceHost, &m.DestinationHost, &m.SourceUsername, &m.DestinationUsername, &m.MessagesTotal, &m.MessagesCopied, &m.MessagesFailed, &m.BytesTotal, &m.BytesCopied); err != nil {
			return nil, err
		}
		m.CreatedAt, _ = time.Parse(time.RFC3339Nano, c)
		if f.Valid {
			x, _ := time.Parse(time.RFC3339Nano, f.String)
			m.FinishedAt = &x
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return out, err
	}
	_ = rows.Close()
	for i := range out {
		out[i].Services, _ = d.JobServices(ctx, out[i].ID)
	}
	return out, nil
}

func nullable(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}
func nullableUint(v uint32) interface{} {
	if v == 0 {
		return nil
	}
	return v
}
func firstRune(s string) (rune, int) {
	for _, r := range s {
		return r, 1
	}
	return 0, 0
}
