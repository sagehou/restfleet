package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sagehou/restfleet/internal/domain"
)

func validSnapshotIndexCode(code string) bool {
	switch code {
	case "", "INDEX_FAILED", "INDEX_TIMED_OUT", "REPOSITORY_UNAVAILABLE", "REPOSITORY_LOCKED",
		"PASSWORD_REJECTED", "CREDENTIAL_CHANGED", "CREDENTIAL_DISABLED", "SECRET_UNAVAILABLE",
		"CONFIG_UNSAFE", "REFRESH_FAILED", "WORKER_LOST":
		return true
	}
	return false
}

// EnqueueSnapshotIndex creates one system-owned read-only index operation.
// It uses the same repository lock and active-operation guard as SNAPSHOT_LIST.
func (s *Store) EnqueueSnapshotIndex(ctx context.Context, operation domain.Operation) (domain.Operation, error) {
	if operation.RepositoryID == nil || operation.ID.Version() != 7 || operation.RequestedByUserID != nil {
		return operation, domain.ErrOperationTransition
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
	operation.Type, operation.Status, operation.Source = "SNAPSHOT_INDEX", "QUEUED", "SYSTEM"
	operation.StorageCredentialID, operation.SecretRevision = credential.ID, credential.SecretRevision
	operation.Attempt, operation.CreatedAt = 1, now
	if _, err = tx.Exec(ctx, `insert into operations(id,type,status,source,storage_credential_id,secret_revision,
		requested_by_user_id,attempt,created_at,repository_id)
		values($1,$2,$3,$4,$5,$6,null,$7,$8,$9)`,
		operation.ID, operation.Type, operation.Status, operation.Source, operation.StorageCredentialID,
		operation.SecretRevision, operation.Attempt, operation.CreatedAt, repo.ID); err != nil {
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
		values($1,$2,'SNAPSHOT_INDEX','READY',$3,'{}'::jsonb,$3,$3)`, jobID, operation.ID, now); err != nil {
		return operation, err
	}
	if err = recordOperationEvent(ctx, tx, operation, "", now); err != nil {
		return operation, err
	}
	return operation, tx.Commit(ctx)
}

// ReadyRepositoryIDs returns initialized repositories eligible for a periodic
// index. A caller still checks the active-operation guard before enqueueing.
func (s *Store) ReadyRepositoryIDs(ctx context.Context, limit int) ([]uuid.UUID, error) {
	rows, err := s.pool.Query(ctx, `select id from repositories
		where archived_at is null and status='READY' and restic_id is not null
		order by id limit $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := make([]uuid.UUID, 0)
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// FinishSnapshotIndexJob upserts a complete index in one transaction. Missing
// markers are only written when the entire `restic snapshots` run succeeded.
func (s *Store) FinishSnapshotIndexJob(ctx context.Context, id, owner uuid.UUID, code string, result domain.SnapshotIndexResult) error {
	if !validSnapshotIndexCode(code) {
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
	if job.Operation.Type != "SNAPSHOT_INDEX" || job.Repository == nil {
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
		if len(result.Entries) > 50_000 {
			return domain.ErrOperationTransition
		}
		seen := make([]string, 0, len(result.Entries))
		for _, entry := range result.Entries {
			if len(entry.SnapshotID) != 64 || len(entry.TreeID) != 64 ||
				entry.CapturedAt.IsZero() || len(entry.Tags) > 100 || len(entry.Paths) > 1_000 {
				return domain.ErrOperationTransition
			}
			var planID, agentID any
			hostID := job.Repository.HostID
			managed := entry.PlanID != nil
			if managed {
				var candidateHost, candidateAgent uuid.UUID
				err := tx.QueryRow(ctx, `select host_id,agent_id from plans
					where id=$1 and repository_id=$2 and archived_at is null`,
					*entry.PlanID, job.Repository.ID).Scan(&candidateHost, &candidateAgent)
				if errors.Is(err, pgx.ErrNoRows) {
					managed = false
				} else if err != nil {
					return err
				} else {
					hostID, planID, agentID = candidateHost, *entry.PlanID, candidateAgent
				}
			}
			tags, paths := entry.Tags, entry.Paths
			if tags == nil {
				tags = []string{}
			}
			if paths == nil {
				paths = []string{}
			}
			if _, err = tx.Exec(ctx, `insert into snapshots(repository_id,snapshot_id,plan_id,agent_id,host_id,
				operation_id,captured_at,recorded_at,files_new,bytes_added,managed,tree_id,tags,paths,program_version,missing_at)
				values($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,null)
				on conflict (repository_id,snapshot_id) do update set
					plan_id=excluded.plan_id,agent_id=excluded.agent_id,host_id=excluded.host_id,
					operation_id=excluded.operation_id,captured_at=excluded.captured_at,recorded_at=excluded.recorded_at,
					files_new=excluded.files_new,bytes_added=excluded.bytes_added,managed=excluded.managed,
					tree_id=excluded.tree_id,tags=excluded.tags,paths=excluded.paths,
					program_version=excluded.program_version,missing_at=null`,
				job.Repository.ID, entry.SnapshotID, planID, agentID, hostID, job.Operation.ID,
				entry.CapturedAt, now, entry.FilesNew, entry.BytesAdded, managed, entry.TreeID,
				tags, paths, entry.ProgramVersion); err != nil {
				return err
			}
			seen = append(seen, entry.SnapshotID)
		}
		if _, err = tx.Exec(ctx, `update snapshots set missing_at=$2
			where repository_id=$1 and missing_at is null and not (snapshot_id = any($3::text[]))`,
			job.Repository.ID, now, seen); err != nil {
			return err
		}
		to = "SUCCEEDED"
	} else if code == "INDEX_TIMED_OUT" {
		to = "TIMED_OUT"
	}
	job.Operation.ErrorCode = code
	if err = transitionOperation(ctx, tx, &job.Operation, to, now); err != nil {
		return err
	}
	if err = ensureCredentialLease(ctx, tx, id, owner); err != nil {
		return err
	}
	if err = finishJobRow(ctx, tx, &job, to, now); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, "delete from repository_leases where repository_id=$1 and operation_id=$2",
		job.Repository.ID, job.Operation.ID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
