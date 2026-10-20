package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sagehou/restfleet/internal/domain"
)

const snapshotListLifetime = 5 * time.Minute

func optionalUUID(value *uuid.UUID) any {
	if value == nil {
		return nil
	}
	return *value
}

// lockRepositoryForRead locks the host and repository rows for a read-only
// snapshot job. Unlike provisioning, an initialized READY repository is valid.
func lockRepositoryForRead(ctx context.Context, tx pgx.Tx, id uuid.UUID) (domain.Repository, error) {
	var hostID uuid.UUID
	err := tx.QueryRow(ctx, "select host_id from repositories where id=$1", id).Scan(&hostID)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Repository{}, domain.ErrNotFound
	}
	if err != nil {
		return domain.Repository{}, err
	}
	var status string
	if err = tx.QueryRow(ctx, "select status from hosts where id=$1 for update", hostID).Scan(&status); err != nil {
		return domain.Repository{}, err
	}
	r, err := scanRepository(tx.QueryRow(ctx, "select "+repositoryColumns+" from repositories where id=$1 for update", id))
	if err != nil {
		return domain.Repository{}, err
	}
	if status != "PENDING" && status != "ACTIVE" {
		return r, domain.ErrHostUnavailable
	}
	return r, nil
}

// EnqueueSnapshotList creates one read-only scan job. It reuses the existing
// repository serialization: no active operation, backup admission or live lease
// may exist, so credential refresh remains safe.
func (s *Store) EnqueueSnapshotList(ctx context.Context, operation domain.Operation, snapshotID, path string) (domain.Operation, error) {
	if operation.RepositoryID == nil || operation.RequestedByUserID == uuid.Nil ||
		operation.ID.Version() != 7 || len(path) == 0 || len(path) > 4096 {
		return operation, domain.ErrOperationTransition
	}
	payload, err := json.Marshal(map[string]string{"snapshot_id": snapshotID, "path": path})
	if err != nil {
		return operation, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return operation, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	repo, err := lockRepositoryForRead(ctx, tx, *operation.RepositoryID)
	if err != nil {
		return operation, err
	}
	if repo.ResticID == "" || repo.InitializedAt == nil {
		return operation, domain.ErrRepositoryUnavailable
	}
	var active bool
	if err = tx.QueryRow(ctx, `select exists(select 1 from operations
		where repository_id=$1 and finished_at is null)`, repo.ID).Scan(&active); err != nil {
		return operation, err
	}
	if active {
		return operation, domain.ErrRepositoryBusy
	}
	if err = tx.QueryRow(ctx, `select exists(select 1 from repository_leases
		where repository_id=$1 and expires_at>clock_timestamp())`, repo.ID).Scan(&active); err != nil {
		return operation, err
	}
	if active {
		return operation, domain.ErrRepositoryBusy
	}
	credential, err := scanCredential(tx.QueryRow(ctx, "select "+credentialColumns+" from storage_credentials where id=$1 for update", repo.StorageCredentialID))
	if err != nil {
		return operation, err
	}
	if credential.Status == "DISABLED" {
		return operation, domain.ErrCredentialDisabled
	}
	if err = ensureNoBackupAdmission(ctx, tx, credential.ID); err != nil {
		return operation, err
	}
	var now time.Time
	if err = tx.QueryRow(ctx, "select clock_timestamp()").Scan(&now); err != nil {
		return operation, err
	}
	operation.Type, operation.Status, operation.Source = "SNAPSHOT_LIST", "QUEUED", "USER"
	operation.StorageCredentialID, operation.SecretRevision = credential.ID, credential.SecretRevision
	operation.Attempt, operation.CreatedAt = 1, now
	if _, err = tx.Exec(ctx, `insert into operations(id,type,status,source,storage_credential_id,secret_revision,
		requested_by_user_id,attempt,created_at,repository_id)
		values($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		operation.ID, operation.Type, operation.Status, operation.Source, operation.StorageCredentialID,
		operation.SecretRevision, operation.RequestedByUserID, operation.Attempt, operation.CreatedAt, repo.ID); err != nil {
		var databaseError *pgconn.PgError
		if errors.As(err, &databaseError) && databaseError.Code == "23505" {
			return operation, domain.ErrRepositoryBusy
		}
		return operation, err
	}
	jobID, err := uuid.NewV7()
	if err != nil {
		return operation, err
	}
	if _, err = tx.Exec(ctx, `insert into jobs(id,operation_id,queue,status,available_at,payload,created_at,updated_at)
		values($1,$2,'SNAPSHOT_LIST','READY',$3,$4,$3,$3)`, jobID, operation.ID, now, payload); err != nil {
		return operation, err
	}
	if err = recordOperationEvent(ctx, tx, operation, "", now); err != nil {
		return operation, err
	}
	return operation, tx.Commit(ctx)
}

func (s *Store) ActiveSnapshotListOperation(ctx context.Context, repositoryID uuid.UUID) (domain.Operation, bool, error) {
	operation, err := scanOperation(s.pool.QueryRow(ctx, "select "+operationColumns+` from operations
		where repository_id=$1 and type='SNAPSHOT_LIST' and finished_at is null`, repositoryID))
	if errors.Is(err, domain.ErrNotFound) {
		return domain.Operation{}, false, nil
	}
	if err != nil {
		return domain.Operation{}, false, err
	}
	return operation, true, nil
}

// SnapshotListResult returns a cached page only while it is fresh. Expired rows
// are never served and may be removed by the next scan.
func (s *Store) SnapshotListResult(ctx context.Context, repositoryID uuid.UUID, snapshotID, path string, now time.Time) (domain.SnapshotListResult, bool, error) {
	var result domain.SnapshotListResult
	var entries []byte
	err := s.pool.QueryRow(ctx, `select operation_id,repository_id,snapshot_id,tree_id,path,truncated,scanned,entries,collected_at
		from snapshot_list_results where repository_id=$1 and snapshot_id=$2 and path=$3 and expires_at>$4
		order by collected_at desc limit 1`, repositoryID, snapshotID, path, now).
		Scan(&result.OperationID, &result.RepositoryID, &result.SnapshotID, &result.TreeID, &result.Path,
			&result.Truncated, &result.Scanned, &entries, &result.CollectedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.SnapshotListResult{}, false, nil
	}
	if err != nil {
		return domain.SnapshotListResult{}, false, err
	}
	if err = json.Unmarshal(entries, &result.Entries); err != nil {
		return domain.SnapshotListResult{}, false, err
	}
	return result, true, nil
}

func validSnapshotListCode(code string) bool {
	switch code {
	case "", "SNAPSHOT_NOT_FOUND", "PATH_INVALID", "SCAN_FAILED", "SCAN_TIMED_OUT",
		"REPOSITORY_UNAVAILABLE", "REPOSITORY_LOCKED", "PASSWORD_REJECTED",
		"CREDENTIAL_CHANGED", "CREDENTIAL_DISABLED", "SECRET_UNAVAILABLE",
		"CONFIG_UNSAFE", "REFRESH_FAILED", "WORKER_LOST":
		return true
	}
	return false
}

// FinishSnapshotListJob persists the bounded page and completes the operation in
// one transaction. A failed scan stores no partial page.
func (s *Store) FinishSnapshotListJob(ctx context.Context, id, owner uuid.UUID, code string, result domain.SnapshotListResult) error {
	if !validSnapshotListCode(code) {
		return domain.ErrOperationTransition
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	job, err := lockCredentialJob(ctx, tx, id, owner)
	if err != nil {
		return err
	}
	if job.Operation.Type != "SNAPSHOT_LIST" || job.Repository == nil {
		return domain.ErrOperationTransition
	}
	var now time.Time
	if err = tx.QueryRow(ctx, "select clock_timestamp()").Scan(&now); err != nil {
		return err
	}
	if !credentialUnchanged(job) {
		code = "CREDENTIAL_CHANGED"
		if job.Credential.Status == "DISABLED" {
			code = "CREDENTIAL_DISABLED"
		}
	}
	if !job.RepositoryAvailable {
		code = "REPOSITORY_UNAVAILABLE"
	}
	to := "FAILED"
	if code == "" {
		if len(result.SnapshotID) != 64 || len(result.TreeID) != 64 || len(result.Path) == 0 ||
			len(result.Path) > 4096 || len(result.Entries) > 5000 || result.Scanned < 0 {
			return domain.ErrOperationTransition
		}
		result.RepositoryID = job.Repository.ID
		result.CollectedAt = now
		if result.Entries == nil {
			result.Entries = make([]domain.SnapshotEntry, 0)
		}
		entries, err := json.Marshal(result.Entries)
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `delete from snapshot_list_results
			where repository_id=$1 and snapshot_id=$2 and path=$3`, job.Repository.ID, result.SnapshotID, result.Path); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `insert into snapshot_list_results(operation_id,repository_id,snapshot_id,tree_id,path,
			truncated,scanned,entries,collected_at,expires_at) values($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
			job.Operation.ID, job.Repository.ID, result.SnapshotID, result.TreeID, result.Path,
			result.Truncated, result.Scanned, entries, now, now.Add(snapshotListLifetime)); err != nil {
			return err
		}
		to = "SUCCEEDED"
		if result.Truncated {
			to = "SUCCEEDED_WITH_WARNINGS"
		}
	} else if code == "SCAN_TIMED_OUT" {
		to = "TIMED_OUT"
	}
	job.Operation.ErrorCode = code
	if err = transitionOperation(ctx, tx, &job.Operation, to, now); err != nil {
		return err
	}
	if err = ensureCredentialLease(ctx, tx, id, owner); err != nil {
		return err
	}
	return finishJobRow(ctx, tx, &job, to, now)
}

// SnapshotsByID resolves every index entry for one native snapshot ID. The API
// requires a repository when more than one repository contains the ID.
func (s *Store) SnapshotsByID(ctx context.Context, snapshotID string) ([]domain.Snapshot, error) {
	rows, err := s.pool.Query(ctx, `select plan_id,snapshot_id,agent_id,host_id,repository_id,operation_id,captured_at,recorded_at,files_new,bytes_added
		from snapshots where snapshot_id=$1 order by recorded_at desc`, snapshotID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]domain.Snapshot, 0)
	for rows.Next() {
		var item domain.Snapshot
		if err := rows.Scan(&item.PlanID, &item.SnapshotID, &item.AgentID, &item.HostID, &item.RepositoryID,
			&item.OperationID, &item.CapturedAt, &item.RecordedAt, &item.FilesNew, &item.BytesAdded); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}
