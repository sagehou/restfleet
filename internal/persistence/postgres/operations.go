package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/sagehou/restfleet/internal/domain"
)

const backupOperationColumns = "id,plan_id,agent_id,type,source,status,plan_revision,config_hash,scheduled_at,started_at,finished_at,exit_code,error_code,snapshot_id,files_done,total_files,bytes_done,total_bytes,created_at,updated_at,parent_operation_id,root_operation_id,cancel_requested_at,cancel_reason,dispatched_at"

func scanBackupOperation(row rowScanner) (domain.BackupOperation, error) {
	var o domain.BackupOperation
	err := row.Scan(&o.ID, &o.PlanID, &o.AgentID, &o.Type, &o.Source, &o.Status, &o.PlanRevision, &o.ConfigHash,
		&o.ScheduledAt, &o.StartedAt, &o.FinishedAt, &o.ExitCode, &o.ErrorCode, &o.SnapshotID,
		&o.FilesDone, &o.TotalFiles, &o.BytesDone, &o.TotalBytes, &o.CreatedAt, &o.UpdatedAt,
		&o.ParentOperationID, &o.RootOperationID, &o.CancelRequestedAt, &o.CancelReason, &o.DispatchedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return o, domain.ErrNotFound
	}
	return o, err
}

func (s *Store) BackupOperation(ctx context.Context, id uuid.UUID) (domain.BackupOperation, error) {
	return scanBackupOperation(s.pool.QueryRow(ctx, "select "+backupOperationColumns+" from backup_operations where id=$1", id))
}

func (s *Store) BackupOperations(ctx context.Context, after uuid.UUID, limit int) ([]domain.BackupOperation, error) {
	query := "select " + backupOperationColumns + " from backup_operations order by id desc limit $1"
	args := []any{limit}
	if after != uuid.Nil {
		query = "select " + backupOperationColumns + " from backup_operations where id<$1 order by id desc limit $2"
		args = []any{after, limit}
	}
	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]domain.BackupOperation, 0)
	for rows.Next() {
		operation, err := scanBackupOperation(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, operation)
	}
	return items, rows.Err()
}

// UpsertOperationProgress creates the row for a locally scheduled run the first
// time the Agent reports it, then stores the throttled counters.
func (s *Store) UpsertOperationProgress(ctx context.Context, progress domain.BackupOperationProgress) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var exists bool
	err = tx.QueryRow(ctx, `select exists(select 1 from plans p join agents a on a.id=$2 and a.host_id=p.host_id
		where p.id=$1)`, progress.PlanID, progress.AgentID).Scan(&exists)
	if err != nil {
		return err
	}
	if !exists {
		return domain.ErrPlanBindingInvalid
	}
	if _, err = tx.Exec(ctx, `insert into backup_operations(id,plan_id,agent_id,type,source,status,plan_revision,config_hash,scheduled_at,started_at,created_at,updated_at)
		values($1,$2,$3,'BACKUP','SCHEDULED','RUNNING',$4,'',$5,$6,$6,$6)
		on conflict (id) do update set status=case when backup_operations.status='CANCEL_REQUESTED' then 'CANCEL_REQUESTED' else 'RUNNING' end,
			updated_at=excluded.updated_at,started_at=coalesce(backup_operations.started_at,excluded.started_at)
		where backup_operations.finished_at is null`,
		progress.OperationID, progress.PlanID, progress.AgentID, progress.PlanRevision, progress.ScheduledAt, progress.ObservedAt); err != nil {
		return persistenceError(err)
	}
	if _, err = tx.Exec(ctx, `insert into backup_operation_events(operation_id,sequence,stage,files_done,total_files,bytes_done,total_bytes,observed_at)
		values($1,$2,$3,$4,$5,$6,$7,$8) on conflict (operation_id,sequence) do nothing`,
		progress.OperationID, progress.Sequence, progress.Stage, progress.FilesDone, progress.TotalFiles,
		progress.BytesDone, progress.TotalBytes, progress.ObservedAt); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `update backup_operations set files_done=$2,total_files=$3,bytes_done=$4,total_bytes=$5,updated_at=$6
		where id=$1 and finished_at is null`, progress.OperationID, progress.FilesDone, progress.TotalFiles,
		progress.BytesDone, progress.TotalBytes, progress.ObservedAt); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// UpsertOperationResult writes the first terminal state and indexes its snapshot
// in the same transaction. A duplicate or late result is acknowledged without
// rewriting a terminal row; a failed index write rolls the result back for replay.
func (s *Store) UpsertOperationResult(ctx context.Context, result domain.BackupOperationResult) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var hostID, repositoryID uuid.UUID
	err = tx.QueryRow(ctx, `select p.host_id, p.repository_id from plans p
		join agents a on a.id=$2 and a.host_id=p.host_id where p.id=$1`,
		result.PlanID, result.AgentID).Scan(&hostID, &repositoryID)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.ErrPlanBindingInvalid
	}
	if err != nil {
		return err
	}
	finished := result.FinishedAt
	tag, err := tx.Exec(ctx, `insert into backup_operations(id,plan_id,agent_id,type,source,status,plan_revision,config_hash,
		scheduled_at,started_at,finished_at,exit_code,error_code,snapshot_id,created_at,updated_at)
		values($1,$2,$3,'BACKUP','SCHEDULED',$4,$5,'',$6,$6,$7,$8,$9,$10,$7,$7)
		on conflict (id) do update set status=excluded.status,finished_at=excluded.finished_at,exit_code=excluded.exit_code,
			error_code=excluded.error_code,snapshot_id=excluded.snapshot_id,updated_at=excluded.updated_at
		where backup_operations.finished_at is null`,
		result.OperationID, result.PlanID, result.AgentID, result.Status, result.PlanRevision, result.ScheduledAt,
		finished, result.ExitCode, result.ErrorCode, result.SnapshotID)
	if err != nil {
		return persistenceError(err)
	}
	// A terminal row is authoritative: duplicate or late results may be
	// acknowledged, but must not rewrite it or regress plan health.
	if tag.RowsAffected() == 0 {
		return tx.Commit(ctx)
	}
	health := ""
	switch result.Status {
	case "SUCCEEDED", "SUCCEEDED_WITH_WARNINGS":
		health = "HEALTHY"
	case "FAILED":
		health = "FAILED"
	}
	if health != "" {
		if _, err = tx.Exec(ctx, "update plans set backup_health=$2,updated_at=$3 where id=$1", result.PlanID, health, finished); err != nil {
			return err
		}
	}
	if result.SnapshotID != "" {
		if _, err = tx.Exec(ctx, `insert into snapshots(plan_id,snapshot_id,agent_id,host_id,repository_id,
			operation_id,captured_at,recorded_at,files_new,bytes_added)
			values($1,$2,$3,$4,$5,$6,$7,now(),$8,$9)
			on conflict (plan_id,snapshot_id) do nothing`,
			result.PlanID, result.SnapshotID, result.AgentID, hostID, repositoryID,
			result.OperationID, finished, result.FilesNew, result.DataAdded); err != nil {
			return persistenceError(err)
		}
	}
	return tx.Commit(ctx)
}

var _ = time.Now

// FleetBackupCounts feeds the dashboard without loading individual rows.
func (s *Store) FleetBackupCounts(ctx context.Context) (plans, operations, running, failed24h, unhealthy int64, err error) {
	err = s.pool.QueryRow(ctx, `select
		(select count(*) from plans where archived_at is null),
		(select count(*) from backup_operations),
		(select count(*) from backup_operations where status='RUNNING'),
		(select count(*) from backup_operations where status='FAILED' and finished_at > now() - interval '24 hours'),
		(select count(*) from plans where archived_at is null and backup_health='FAILED')`).
		Scan(&plans, &operations, &running, &failed24h, &unhealthy)
	return
}

func (s *Store) Snapshots(ctx context.Context, limit int) ([]domain.Snapshot, error) {
	rows, err := s.pool.Query(ctx, `select plan_id,snapshot_id,agent_id,host_id,repository_id,operation_id,captured_at,recorded_at,files_new,bytes_added
		from snapshots order by recorded_at desc limit $1`, limit)
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

// CreateManualOperation queues a user-triggered backup for one Agent.
func (s *Store) CreateManualOperation(ctx context.Context, operation domain.BackupOperation) error {
	_, err := s.pool.Exec(ctx, `insert into backup_operations(id,plan_id,agent_id,type,source,status,plan_revision,config_hash,scheduled_at,created_at,updated_at,parent_operation_id,root_operation_id)
		values($1,$2,$3,'BACKUP','MANUAL','QUEUED',$4,$5,$6,$7,$7,$8,$9)`,
		operation.ID, operation.PlanID, operation.AgentID, operation.PlanRevision, operation.ConfigHash,
		operation.ScheduledAt, operation.CreatedAt, optionalUUID(operation.ParentOperationID), optionalUUID(operation.RootOperationID))
	return persistenceError(err)
}

// CancelBackupOperation closes an operation the dispatcher has not claimed and
// asks the Agent to stop one that may already be on the wire. A never-dispatched
// QUEUED row can be closed safely; a claimed row must stay non-terminal until
// the Agent proves it stopped, so a concurrent dispatch can never execute
// unnoticed. The terminal result remains authoritative.
func (s *Store) CancelBackupOperation(ctx context.Context, id uuid.UUID, reason string, now time.Time) (domain.BackupOperation, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return domain.BackupOperation{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	operation, err := scanBackupOperation(tx.QueryRow(ctx, "select "+backupOperationColumns+" from backup_operations where id=$1 for update", id))
	if err != nil {
		return domain.BackupOperation{}, err
	}
	switch operation.Status {
	case "CANCELED", "SUCCEEDED", "SUCCEEDED_WITH_WARNINGS", "FAILED", "LOST", "MISSED", "CONCURRENCY_SKIPPED":
		return domain.BackupOperation{}, domain.ErrOperationTerminal
	case "CANCEL_REQUESTED":
		return operation, tx.Commit(ctx)
	case "QUEUED":
		if operation.DispatchedAt == nil {
			operation.Status, operation.FinishedAt = "CANCELED", &now
			break
		}
		operation.Status = "CANCEL_REQUESTED"
	default:
		operation.Status = "CANCEL_REQUESTED"
	}
	operation.CancelRequestedAt, operation.CancelReason, operation.UpdatedAt = &now, reason, now
	var finished any
	if operation.FinishedAt != nil {
		finished = *operation.FinishedAt
	}
	if _, err = tx.Exec(ctx, `update backup_operations set status=$2,finished_at=$3,cancel_requested_at=$4,cancel_reason=$5,updated_at=$4,error_code=case when $2='CANCELED' then 'CANCELED' else error_code end
		where id=$1 and finished_at is null`, id, operation.Status, finished, now, reason); err != nil {
		return domain.BackupOperation{}, err
	}
	return operation, tx.Commit(ctx)
}

// ClaimManualOperationDispatch atomically marks a QUEUED manual operation as
// dispatched before the caller writes JobDispatch to the stream. A cancel that
// commits first leaves the row CANCELED (or CANCEL_REQUESTED with no claim), so
// the UPDATE matches nothing and no job reaches the Agent. The row stays QUEUED
// so a lost dispatch is retried after retryAfter.
func (s *Store) ClaimManualOperationDispatch(ctx context.Context, agentID uuid.UUID, now time.Time, retryAfter time.Duration) (domain.BackupOperation, bool, error) {
	retryBefore := now.Add(-retryAfter)
	operation, err := scanBackupOperation(s.pool.QueryRow(ctx, `update backup_operations set dispatched_at=$2,updated_at=$2
		where id = (
			select id from backup_operations
			where agent_id=$1 and source='MANUAL' and status='QUEUED'
			  and (dispatched_at is null or dispatched_at <= $3)
			order by created_at
			limit 1
			for update skip locked
		)
		returning `+backupOperationColumns, agentID, now, retryBefore))
	if errors.Is(err, domain.ErrNotFound) {
		return domain.BackupOperation{}, false, nil
	}
	if err != nil {
		return domain.BackupOperation{}, false, err
	}
	return operation, true, nil
}

// CloseUnstartedCancellations terminates a cancellation that the Agent never
// acknowledged as started. Without this closure a cancel racing a claimed but
// undelivered JobDispatch would leave the row CANCEL_REQUESTED forever and the
// Agent could never prove a run that never existed.
func (s *Store) CloseUnstartedCancellations(ctx context.Context, cutoff, now time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, `update backup_operations
		set status='CANCELED',error_code='CANCELED',finished_at=$2,updated_at=$2
		where status='CANCEL_REQUESTED' and started_at is null and finished_at is null
		  and cancel_requested_at is not null and cancel_requested_at <= $1`, cutoff, now)
	if err != nil {
		return 0, persistenceError(err)
	}
	return tag.RowsAffected(), nil
}

func (s *Store) PendingCancelOperations(ctx context.Context, agentID uuid.UUID) ([]domain.BackupOperation, error) {
	rows, err := s.pool.Query(ctx, "select "+backupOperationColumns+` from backup_operations
		where agent_id=$1 and status='CANCEL_REQUESTED' order by created_at`, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]domain.BackupOperation, 0)
	for rows.Next() {
		operation, err := scanBackupOperation(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, operation)
	}
	return items, rows.Err()
}

// RejectManualOperation closes a dispatched manual job the Agent refused.
func (s *Store) RejectManualOperation(ctx context.Context, id uuid.UUID, now time.Time) error {
	_, err := s.pool.Exec(ctx, `update backup_operations
		set status='FAILED',error_code='AGENT_REJECTED',finished_at=$2,updated_at=$2
		where id=$1 and source='MANUAL' and status in ('QUEUED','RUNNING','CANCEL_REQUESTED') and finished_at is null`, id, now)
	return err
}

// MarkOperationDispatched records JobAccepted. A cancel that arrived first
// leaves the row CANCEL_REQUESTED, which this transition deliberately ignores.
func (s *Store) MarkOperationDispatched(ctx context.Context, id uuid.UUID) error {
	_, err := s.pool.Exec(ctx, "update backup_operations set status='RUNNING',started_at=coalesce(started_at,now()),updated_at=now() where id=$1 and status='QUEUED'", id)
	return err
}
