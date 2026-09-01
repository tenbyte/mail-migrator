package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"time"

	"github.com/tenbyte/mail-migrator/internal/domain"
)

type DAVCollectionRecord struct {
	ID, MigrationID                       int64
	Kind                                  domain.ServiceKind
	SourcePath, SourceName                string
	SourceDescription                     string
	DestinationPath, DestinationName      string
	SourceSyncToken, DestinationSyncToken string
	Enabled                               bool
}

func (d *DB) CreateJob(ctx context.Context, request domain.StartJobRequest) (int64, error) {
	base := domain.StartRequest{Source: request.MailSource, Destination: request.MailDestination, Mappings: request.MailMappings, Options: request.Options}
	if !request.MailEnabled {
		base.Source = domain.AccountConfig{Host: endpointLabel(request.Calendar.Source, request.Contacts.Source), Port: 443, Encryption: domain.EncryptionTLS}
		base.Destination = domain.AccountConfig{Host: endpointLabel(request.Calendar.Destination, request.Contacts.Destination), Port: 443, Encryption: domain.EncryptionTLS}
		base.Mappings = nil
	}
	id, err := d.CreateMigration(ctx, base)
	if err != nil {
		return 0, err
	}
	if err := d.ConfigureJob(ctx, id, request); err != nil {
		return 0, err
	}
	return id, nil
}

func endpointLabel(endpoints ...domain.DAVEndpoint) string {
	for _, endpoint := range endpoints {
		if endpoint.URL == "" {
			continue
		}
		if parsed, err := url.Parse(endpoint.URL); err == nil && parsed.Host != "" {
			return parsed.Host
		}
		return endpoint.URL
	}
	return "DAV"
}

func (d *DB) ConfigureJob(ctx context.Context, migrationID int64, request domain.StartJobRequest) error {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if !request.MailEnabled {
		if _, err := tx.ExecContext(ctx, `DELETE FROM migration_services WHERE migration_id=? AND kind=?`, migrationID, domain.ServiceMail); err != nil {
			return err
		}
	} else {
		if _, err := tx.ExecContext(ctx, `UPDATE migration_services SET source_url=?,source_username=?,source_credential_id=?,destination_url=?,destination_username=?,destination_credential_id=? WHERE migration_id=? AND kind=?`, request.MailSource.Host, request.MailSource.Username, nullable(request.MailSource.CredentialID), request.MailDestination.Host, request.MailDestination.Username, nullable(request.MailDestination.CredentialID), migrationID, domain.ServiceMail); err != nil {
			return err
		}
	}
	for _, service := range []domain.DAVServiceRequest{request.Calendar, request.Contacts} {
		if !service.Enabled {
			continue
		}
		settings, _ := json.Marshal(struct {
			SourceAuth      string                 `json:"sourceAuth"`
			DestinationAuth string                 `json:"destinationAuth"`
			TransferOptions domain.TransferOptions `json:"transferOptions"`
		}{SourceAuth: string(service.Source.AuthMethod), DestinationAuth: string(service.Destination.AuthMethod), TransferOptions: request.Options})
		var items, bytes int64
		for _, mapping := range service.Mappings {
			if mapping.Enabled {
				items += mapping.Source.Objects
				bytes += mapping.Source.Bytes
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO migration_services(migration_id,kind,status,source_url,source_username,source_credential_id,destination_url,destination_username,destination_credential_id,items_total,bytes_total,options_json) VALUES(?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(migration_id,kind) DO UPDATE SET status=excluded.status,source_url=excluded.source_url,source_username=excluded.source_username,source_credential_id=excluded.source_credential_id,destination_url=excluded.destination_url,destination_username=excluded.destination_username,destination_credential_id=excluded.destination_credential_id,items_total=excluded.items_total,bytes_total=excluded.bytes_total,options_json=excluded.options_json`, migrationID, service.Kind, domain.MigrationReady, service.Source.URL, service.Source.Username, nullable(service.Source.CredentialID), service.Destination.URL, service.Destination.Username, nullable(service.Destination.CredentialID), items, bytes, string(settings)); err != nil {
			return err
		}
		for _, mapping := range service.Mappings {
			if _, err := tx.ExecContext(ctx, `INSERT INTO dav_collections(migration_id,kind,source_path,source_name,source_description,destination_path,destination_name,source_sync_token,status,enabled) VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT(migration_id,kind,source_path) DO UPDATE SET source_name=excluded.source_name,source_description=excluded.source_description,destination_path=excluded.destination_path,destination_name=excluded.destination_name,enabled=excluded.enabled`, migrationID, service.Kind, mapping.Source.Path, mapping.Source.Name, mapping.Source.Description, mapping.DestinationPath, mapping.DestinationName, mapping.Source.SyncToken, domain.MessagePending, mapping.Enabled); err != nil {
				return err
			}
		}
	}
	var totalItems, totalBytes int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(items_total),0),COALESCE(SUM(bytes_total),0) FROM migration_services WHERE migration_id=?`, migrationID).Scan(&totalItems, &totalBytes); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE migrations SET messages_total=?,bytes_total=?,status=?,finished_at=NULL,last_error='' WHERE id=?`, totalItems, totalBytes, domain.MigrationReady, migrationID); err != nil {
		return err
	}
	return tx.Commit()
}

func (d *DB) DAVCollections(ctx context.Context, migrationID int64, kind domain.ServiceKind) ([]DAVCollectionRecord, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT id,migration_id,kind,source_path,source_name,source_description,destination_path,destination_name,source_sync_token,destination_sync_token,enabled FROM dav_collections WHERE migration_id=? AND kind=? ORDER BY source_name,source_path`, migrationID, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]DAVCollectionRecord, 0)
	for rows.Next() {
		var record DAVCollectionRecord
		if err := rows.Scan(&record.ID, &record.MigrationID, &record.Kind, &record.SourcePath, &record.SourceName, &record.SourceDescription, &record.DestinationPath, &record.DestinationName, &record.SourceSyncToken, &record.DestinationSyncToken, &record.Enabled); err != nil {
			return nil, err
		}
		result = append(result, record)
	}
	return result, rows.Err()
}

func (d *DB) LoadDAVServiceRequest(ctx context.Context, migrationID int64, kind domain.ServiceKind) (domain.DAVServiceRequest, error) {
	request := domain.DAVServiceRequest{Kind: kind, Enabled: true}
	var sourceCredential, destinationCredential sql.NullString
	var settings string
	err := d.sql.QueryRowContext(ctx, `SELECT source_url,source_username,source_credential_id,destination_url,destination_username,destination_credential_id,options_json FROM migration_services WHERE migration_id=? AND kind=?`, migrationID, kind).Scan(&request.Source.URL, &request.Source.Username, &sourceCredential, &request.Destination.URL, &request.Destination.Username, &destinationCredential, &settings)
	if err != nil {
		return request, err
	}
	request.Source.CredentialID = sourceCredential.String
	request.Destination.CredentialID = destinationCredential.String
	request.Source.RememberCredential = request.Source.CredentialID != ""
	request.Destination.RememberCredential = request.Destination.CredentialID != ""
	var auth struct {
		SourceAuth      string `json:"sourceAuth"`
		DestinationAuth string `json:"destinationAuth"`
	}
	_ = json.Unmarshal([]byte(settings), &auth)
	request.Source.AuthMethod = domain.DAVAuthMethod(auth.SourceAuth)
	request.Destination.AuthMethod = domain.DAVAuthMethod(auth.DestinationAuth)
	collections, err := d.DAVCollections(ctx, migrationID, kind)
	if err != nil {
		return request, err
	}
	for _, collection := range collections {
		request.Mappings = append(request.Mappings, domain.CollectionMapping{Source: domain.DAVCollection{Path: collection.SourcePath, Name: collection.SourceName, Description: collection.SourceDescription, Kind: kind, SyncToken: collection.SourceSyncToken}, DestinationPath: collection.DestinationPath, DestinationName: collection.DestinationName, Enabled: collection.Enabled})
	}
	return request, nil
}

func (d *DB) LoadJobOptions(ctx context.Context, migrationID int64) domain.TransferOptions {
	options := domain.DefaultTransferOptions()
	rows, err := d.sql.QueryContext(ctx, `SELECT kind,options_json FROM migration_services WHERE migration_id=? ORDER BY CASE kind WHEN 'mail' THEN 1 ELSE 2 END`, migrationID)
	if err != nil {
		return options
	}
	defer rows.Close()
	for rows.Next() {
		var kind domain.ServiceKind
		var raw string
		if err := rows.Scan(&kind, &raw); err != nil || raw == "" {
			continue
		}
		if kind == domain.ServiceMail {
			candidate := domain.TransferOptions{}
			if json.Unmarshal([]byte(raw), &candidate) == nil && candidate.Concurrency > 0 {
				return candidate
			}
			continue
		}
		var payload struct {
			TransferOptions domain.TransferOptions `json:"transferOptions"`
		}
		if json.Unmarshal([]byte(raw), &payload) == nil && payload.TransferOptions.Concurrency > 0 {
			return payload.TransferOptions
		}
	}
	return options
}

func (d *DB) UpdateDAVCredentialIDs(ctx context.Context, migrationID int64, kind domain.ServiceKind, sourceID, destinationID string) error {
	_, err := d.sql.ExecContext(ctx, `UPDATE migration_services SET source_credential_id=?,destination_credential_id=? WHERE migration_id=? AND kind=?`, nullable(sourceID), nullable(destinationID), migrationID, kind)
	return err
}

func (d *DB) UpsertDAVResource(ctx context.Context, migrationID, collectionID int64, kind domain.ServiceKind, href, uid, etag string, size int64) (domain.DAVResourceState, error) {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return domain.DAVResourceState{}, err
	}
	defer tx.Rollback()
	var previousState string
	var previousETag string
	lookupErr := tx.QueryRowContext(ctx, `SELECT status,source_etag FROM dav_resources WHERE migration_id=? AND collection_id=? AND source_href=?`, migrationID, collectionID, href).Scan(&previousState, &previousETag)
	if lookupErr != nil && !errors.Is(lookupErr, sql.ErrNoRows) {
		return domain.DAVResourceState{}, lookupErr
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO dav_resources(migration_id,collection_id,kind,source_href,source_uid,source_etag,size,status,source_seen) VALUES(?,?,?,?,?,?,?,?,1) ON CONFLICT(migration_id,collection_id,source_href) DO UPDATE SET source_uid=excluded.source_uid,source_etag=excluded.source_etag,size=excluded.size,source_seen=1,status=CASE WHEN dav_resources.source_etag<>excluded.source_etag AND dav_resources.status IN ('COPIED','SKIPPED') THEN 'PENDING' ELSE dav_resources.status END`, migrationID, collectionID, kind, href, uid, etag, size, domain.MessagePending); err != nil {
		return domain.DAVResourceState{}, err
	}
	if previousState != "" && previousETag != etag {
		if _, err := tx.ExecContext(ctx, `UPDATE dav_conflicts SET source_etag=?,resolution='',resolved_at=NULL WHERE resource_id=(SELECT id FROM dav_resources WHERE migration_id=? AND collection_id=? AND source_href=?)`, etag, migrationID, collectionID, href); err != nil {
			return domain.DAVResourceState{}, err
		}
	}
	if err := refreshDAVServiceStatsTx(ctx, tx, migrationID, kind); err != nil {
		return domain.DAVResourceState{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.DAVResourceState{}, err
	}
	return d.DAVResourceByHref(ctx, migrationID, collectionID, href)
}

func (d *DB) DAVResourceByHref(ctx context.Context, migrationID, collectionID int64, href string) (domain.DAVResourceState, error) {
	var resource domain.DAVResourceState
	err := d.sql.QueryRowContext(ctx, `SELECT id,migration_id,collection_id,kind,source_href,source_uid,source_etag,source_hash,destination_href,destination_etag,size,status,last_error FROM dav_resources WHERE migration_id=? AND collection_id=? AND source_href=?`, migrationID, collectionID, href).Scan(&resource.ID, &resource.MigrationID, &resource.CollectionID, &resource.Kind, &resource.SourceHref, &resource.SourceUID, &resource.SourceETag, &resource.SourceHash, &resource.DestinationHref, &resource.DestinationETag, &resource.Size, &resource.State, &resource.LastError)
	return resource, err
}

func (d *DB) DAVResources(ctx context.Context, migrationID, collectionID int64) ([]domain.DAVResourceState, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT id,migration_id,collection_id,kind,source_href,source_uid,source_etag,source_hash,destination_href,destination_etag,size,status,last_error FROM dav_resources WHERE migration_id=? AND collection_id=? ORDER BY source_href`, migrationID, collectionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]domain.DAVResourceState, 0)
	for rows.Next() {
		var resource domain.DAVResourceState
		if err := rows.Scan(&resource.ID, &resource.MigrationID, &resource.CollectionID, &resource.Kind, &resource.SourceHref, &resource.SourceUID, &resource.SourceETag, &resource.SourceHash, &resource.DestinationHref, &resource.DestinationETag, &resource.Size, &resource.State, &resource.LastError); err != nil {
			return nil, err
		}
		result = append(result, resource)
	}
	return result, rows.Err()
}

func (d *DB) BeginDAVResource(ctx context.Context, resourceID int64) error {
	_, err := d.sql.ExecContext(ctx, `UPDATE dav_resources SET status=?,attempt_count=attempt_count+1,last_error='' WHERE id=?`, domain.MessageTransferring, resourceID)
	return err
}

func (d *DB) CompleteDAVResource(ctx context.Context, migrationID, resourceID int64, sourceHash, destinationHref, destinationETag string, size int64, repaired bool) error {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var kind domain.ServiceKind
	var previousDestination string
	if err := tx.QueryRowContext(ctx, `SELECT kind,destination_href FROM dav_resources WHERE id=? AND migration_id=?`, resourceID, migrationID).Scan(&kind, &previousDestination); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE dav_resources SET source_hash=?,destination_href=?,destination_etag=?,size=?,status=?,last_error='',copied_at=?,destination_seen=1 WHERE id=?`, sourceHash, destinationHref, destinationETag, size, domain.MessageCopied, time.Now().UTC().Format(time.RFC3339Nano), resourceID); err != nil {
		return err
	}
	if err := refreshDAVServiceStatsTx(ctx, tx, migrationID, kind); err != nil {
		return err
	}
	if repaired {
		if _, err := tx.ExecContext(ctx, `INSERT INTO conversion_warnings(migration_id,resource_id,kind,code,message,created_at) SELECT migration_id,id,kind,'REPAIRED','Deleted destination copy was restored',? FROM dav_resources WHERE id=?`, time.Now().UTC().Format(time.RFC3339Nano), resourceID); err != nil {
			return err
		}
	} else if previousDestination != "" {
		if _, err := tx.ExecContext(ctx, `INSERT INTO conversion_warnings(migration_id,resource_id,kind,code,message,created_at) SELECT migration_id,id,kind,'UPDATED','Changed source object was updated at the destination',? FROM dav_resources WHERE id=?`, time.Now().UTC().Format(time.RFC3339Nano), resourceID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (d *DB) FailDAVResource(ctx context.Context, migrationID, resourceID int64, state domain.MessageState, message string) error {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var kind domain.ServiceKind
	if err := tx.QueryRowContext(ctx, `SELECT kind FROM dav_resources WHERE id=?`, resourceID).Scan(&kind); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE dav_resources SET status=?,last_error=? WHERE id=?`, state, message, resourceID); err != nil {
		return err
	}
	if err := refreshDAVServiceStatsTx(ctx, tx, migrationID, kind); err != nil {
		return err
	}
	if state == domain.MessageFailed {
		if _, err := tx.ExecContext(ctx, `UPDATE migration_services SET last_error=? WHERE migration_id=? AND kind=?`, message, migrationID, kind); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (d *DB) AddConversionWarning(ctx context.Context, migrationID, resourceID int64, kind domain.ServiceKind, code, message string) error {
	_, err := d.sql.ExecContext(ctx, `INSERT INTO conversion_warnings(migration_id,resource_id,kind,code,message,created_at) VALUES(?,?,?,?,?,?)`, migrationID, nullableInt64(resourceID), kind, code, message, time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

func (d *DB) AddConflict(ctx context.Context, migrationID, resourceID int64, kind domain.ServiceKind, sourceETag, destinationETag string) error {
	_, err := d.sql.ExecContext(ctx, `INSERT INTO dav_conflicts(migration_id,resource_id,kind,source_etag,destination_etag,created_at) VALUES(?,?,?,?,?,?) ON CONFLICT(resource_id) DO UPDATE SET source_etag=excluded.source_etag,destination_etag=excluded.destination_etag,resolution='',resolved_at=NULL`, migrationID, resourceID, kind, sourceETag, destinationETag, time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

func (d *DB) Conflicts(ctx context.Context, migrationID int64) ([]domain.Conflict, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT c.id,c.migration_id,c.kind,r.source_href,c.source_etag,c.destination_etag,c.resolution FROM dav_conflicts c JOIN dav_resources r ON r.id=c.resource_id WHERE c.migration_id=? ORDER BY c.id`, migrationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]domain.Conflict, 0)
	for rows.Next() {
		var conflict domain.Conflict
		if err := rows.Scan(&conflict.ID, &conflict.MigrationID, &conflict.Kind, &conflict.ResourceHref, &conflict.SourceETag, &conflict.DestinationETag, &conflict.Resolution); err != nil {
			return nil, err
		}
		result = append(result, conflict)
	}
	return result, rows.Err()
}

func (d *DB) ResolveConflict(ctx context.Context, conflictID int64, resolution string) error {
	if resolution != "source" && resolution != "destination" {
		return errors.New("conflict resolution must be source or destination")
	}
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var resourceID, migrationID int64
	var kind domain.ServiceKind
	if err := tx.QueryRowContext(ctx, `SELECT c.resource_id,c.migration_id,c.kind FROM dav_conflicts c WHERE c.id=?`, conflictID).Scan(&resourceID, &migrationID, &kind); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE dav_conflicts SET resolution=?,resolved_at=? WHERE id=?`, resolution, time.Now().UTC().Format(time.RFC3339Nano), conflictID); err != nil {
		return err
	}
	state := domain.MessageSkipped
	if resolution == "source" {
		state = domain.MessagePending
	}
	if _, err := tx.ExecContext(ctx, `UPDATE dav_resources SET status=? WHERE id=?`, state, resourceID); err != nil {
		return err
	}
	if err := refreshDAVServiceStatsTx(ctx, tx, migrationID, kind); err != nil {
		return err
	}
	return tx.Commit()
}

func (d *DB) SetDAVSyncToken(ctx context.Context, collectionID int64, sourceToken, destinationToken string) error {
	_, err := d.sql.ExecContext(ctx, `UPDATE dav_collections SET source_sync_token=?,destination_sync_token=? WHERE id=?`, sourceToken, destinationToken, collectionID)
	return err
}

func (d *DB) UpdateDAVCollectionDestination(ctx context.Context, collectionID int64, destinationPath, destinationName string) error {
	_, err := d.sql.ExecContext(ctx, `UPDATE dav_collections SET destination_path=?,destination_name=? WHERE id=?`, destinationPath, destinationName, collectionID)
	return err
}

func (d *DB) UpdateDAVSourceIdentity(ctx context.Context, resourceID int64, uid, etag string) error {
	_, err := d.sql.ExecContext(ctx, `UPDATE dav_resources SET source_uid=?,source_etag=? WHERE id=?`, uid, etag, resourceID)
	return err
}

func (d *DB) UpdateServiceCurrent(ctx context.Context, migrationID int64, kind domain.ServiceKind, current string, totalItems, totalBytes int64) error {
	_, err := d.sql.ExecContext(ctx, `UPDATE migration_services SET current_item=?,items_total=MAX(items_total,?),bytes_total=MAX(bytes_total,?) WHERE migration_id=? AND kind=?`, current, totalItems, totalBytes, migrationID, kind)
	return err
}

func (d *DB) MarkService(ctx context.Context, migrationID int64, kind domain.ServiceKind, state domain.MigrationState, message string) error {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	query := `UPDATE migration_services SET status=?,last_error=? WHERE migration_id=? AND kind=?`
	args := []any{state, message, migrationID, kind}
	if state == domain.MigrationCompletedWithErrors && message == "" {
		// A service-level or per-item failure has already stored the useful
		// diagnostic. Finishing the run must not erase it merely because the
		// aggregate completion call has no newer error text.
		query = `UPDATE migration_services SET status=? WHERE migration_id=? AND kind=?`
		args = []any{state, migrationID, kind}
	}
	result, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		if _, err := tx.ExecContext(ctx, `INSERT INTO migration_services(migration_id,kind,status,last_error) VALUES(?,?,?,?)`, migrationID, kind, state, message); err != nil {
			return err
		}
	}
	if err := refreshAggregateTx(ctx, tx, migrationID); err != nil {
		return err
	}
	return tx.Commit()
}

func (d *DB) ServiceProgresses(ctx context.Context, migrationID int64) ([]domain.ServiceProgress, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT kind,status,current_item,items_total,items_done,items_failed,bytes_total,bytes_done,last_error FROM migration_services WHERE migration_id=? ORDER BY CASE kind WHEN 'mail' THEN 1 WHEN 'calendar' THEN 2 ELSE 3 END`, migrationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]domain.ServiceProgress, 0)
	for rows.Next() {
		var progress domain.ServiceProgress
		if err := rows.Scan(&progress.Kind, &progress.State, &progress.Current, &progress.ItemsTotal, &progress.ItemsDone, &progress.ItemsFailed, &progress.BytesTotal, &progress.BytesDone, &progress.LastError); err != nil {
			return nil, err
		}
		result = append(result, progress)
	}
	if err := rows.Err(); err != nil {
		return result, err
	}
	if err := rows.Close(); err != nil {
		return result, err
	}
	for i := range result {
		if result[i].Kind != domain.ServiceMail {
			continue
		}
		_ = d.sql.QueryRowContext(ctx, `SELECT
			COALESCE(SUM(CASE WHEN verification_status='verified' THEN 1 ELSE 0 END),0),
			COALESCE(SUM(CASE WHEN status=? THEN 1 ELSE 0 END),0),
			COALESCE(SUM(CASE WHEN status=? THEN 1 ELSE 0 END),0),
			COALESCE(SUM(CASE WHEN status=? THEN 1 ELSE 0 END),0),
			COALESCE(SUM(CASE WHEN verification_status='failed' THEN 1 ELSE 0 END),0),
			COALESCE(SUM(CASE WHEN verification_status='deduplicated' THEN 1 ELSE 0 END),0)
			FROM messages WHERE migration_id=?`, domain.MessageQuarantined, domain.MessageUnknown, domain.MessageSkipped, migrationID).Scan(&result[i].ItemsVerified, &result[i].ItemsQuarantined, &result[i].ItemsUnknown, &result[i].ItemsSkipped, &result[i].VerificationFailed, &result[i].ItemsDeduplicated)
	}
	return result, nil
}

func refreshAggregateTx(ctx context.Context, tx *sql.Tx, migrationID int64) error {
	rows, err := tx.QueryContext(ctx, `SELECT status,items_total,items_done,items_failed,bytes_total,bytes_done,last_error FROM migration_services WHERE migration_id=?`, migrationID)
	if err != nil {
		return err
	}
	defer rows.Close()
	var states []domain.MigrationState
	var total, done, failed, bytesTotal, bytesDone int64
	var lastError string
	for rows.Next() {
		var state domain.MigrationState
		var serviceError string
		var st, sd, sf, sbt, sbd int64
		if err := rows.Scan(&state, &st, &sd, &sf, &sbt, &sbd, &serviceError); err != nil {
			return err
		}
		states = append(states, state)
		total += st
		done += sd
		failed += sf
		bytesTotal += sbt
		bytesDone += sbd
		if serviceError != "" {
			lastError = serviceError
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	state := aggregateState(states, failed)
	var finished any
	if terminalMigrationState(state) {
		finished = time.Now().UTC().Format(time.RFC3339Nano)
	}
	_, err = tx.ExecContext(ctx, `UPDATE migrations SET status=?,messages_total=?,messages_copied=?,messages_failed=?,bytes_total=?,bytes_copied=?,last_error=?,finished_at=CASE WHEN ? IS NULL THEN NULL ELSE COALESCE(finished_at,?) END WHERE id=?`, state, total, done, failed, bytesTotal, bytesDone, lastError, finished, finished, migrationID)
	return err
}

func aggregateState(states []domain.MigrationState, failed int64) domain.MigrationState {
	if len(states) == 0 {
		return domain.MigrationCreated
	}
	for _, state := range states {
		if state == domain.MigrationRunning {
			return domain.MigrationRunning
		}
	}
	for _, state := range states {
		if state == domain.MigrationPaused {
			return domain.MigrationPaused
		}
	}
	for _, state := range states {
		if !terminalMigrationState(state) {
			if state == domain.MigrationInterrupted {
				return domain.MigrationInterrupted
			}
			return domain.MigrationReady
		}
	}
	for _, state := range states {
		if state == domain.MigrationFailed {
			return domain.MigrationFailed
		}
	}
	if failed > 0 {
		return domain.MigrationCompletedWithErrors
	}
	for _, state := range states {
		if state == domain.MigrationCompletedWithErrors {
			return domain.MigrationCompletedWithErrors
		}
		if state == domain.MigrationCancelled {
			return domain.MigrationCancelled
		}
	}
	return domain.MigrationCompleted
}

func terminalMigrationState(state domain.MigrationState) bool {
	return state == domain.MigrationCompleted || state == domain.MigrationCompletedWithErrors || state == domain.MigrationFailed || state == domain.MigrationCancelled
}

func (d *DB) JobServices(ctx context.Context, migrationID int64) ([]domain.ServiceKind, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT kind FROM migration_services WHERE migration_id=? ORDER BY kind`, migrationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []domain.ServiceKind
	for rows.Next() {
		var kind domain.ServiceKind
		if err := rows.Scan(&kind); err != nil {
			return nil, err
		}
		result = append(result, kind)
	}
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result, rows.Err()
}

func nullableInt64(value int64) any {
	if value == 0 {
		return nil
	}
	return value
}

func (d *DB) ResetDAVInventory(ctx context.Context, collectionID int64) error {
	_, err := d.sql.ExecContext(ctx, `UPDATE dav_resources SET source_seen=0,destination_seen=0 WHERE collection_id=?`, collectionID)
	return err
}

func (d *DB) MarkDAVDestinationSeen(ctx context.Context, resourceID int64) error {
	_, err := d.sql.ExecContext(ctx, `UPDATE dav_resources SET destination_seen=1 WHERE id=?`, resourceID)
	return err
}

func (d *DB) RequeueMissingDAVTargets(ctx context.Context, migrationID, collectionID int64) error {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var kind domain.ServiceKind
	if err := tx.QueryRowContext(ctx, `SELECT kind FROM dav_collections WHERE id=? AND migration_id=?`, collectionID, migrationID).Scan(&kind); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE dav_resources SET status=?,last_error='destination copy missing' WHERE migration_id=? AND collection_id=? AND destination_href<>'' AND destination_seen=0 AND status=?`, domain.MessagePending, migrationID, collectionID, domain.MessageCopied); err != nil {
		return err
	}
	if err := refreshDAVServiceStatsTx(ctx, tx, migrationID, kind); err != nil {
		return err
	}
	return tx.Commit()
}

func refreshDAVServiceStatsTx(ctx context.Context, tx *sql.Tx, migrationID int64, kind domain.ServiceKind) error {
	_, err := tx.ExecContext(ctx, `UPDATE migration_services SET
		items_done=(SELECT COUNT(*) FROM dav_resources WHERE migration_id=? AND kind=? AND status='COPIED'),
		items_failed=(SELECT COUNT(*) FROM dav_resources WHERE migration_id=? AND kind=? AND status='FAILED'),
		bytes_done=(SELECT COALESCE(SUM(size),0) FROM dav_resources WHERE migration_id=? AND kind=? AND status='COPIED')
		WHERE migration_id=? AND kind=?`, migrationID, kind, migrationID, kind, migrationID, kind, migrationID, kind)
	return err
}

func (d *DB) ConflictResolution(ctx context.Context, resourceID int64) (string, error) {
	var resolution string
	err := d.sql.QueryRowContext(ctx, `SELECT resolution FROM dav_conflicts WHERE resource_id=?`, resourceID).Scan(&resolution)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return resolution, err
}

func (d *DB) UpdateDAVDestinationIdentity(ctx context.Context, resourceID int64, href, etag string) error {
	_, err := d.sql.ExecContext(ctx, `UPDATE dav_resources SET destination_href=?,destination_etag=?,destination_seen=1 WHERE id=?`, href, etag, resourceID)
	return err
}

func (d *DB) EnsureMigrationExists(ctx context.Context, migrationID int64) error {
	var value int64
	err := d.sql.QueryRowContext(ctx, `SELECT id FROM migrations WHERE id=?`, migrationID).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("migration %d not found", migrationID)
	}
	return err
}
