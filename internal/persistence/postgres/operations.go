package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/sagehou/restfleet/internal/domain"
)

const backupOperationColumns = "id,plan_id,agent_id,type,source,status,plan_revision,config_hash,scheduled_at,started_at,finished_at,exit_code,error_code,snapshot_id,files_done,total_files,bytes_done,total_bytes,created_at,updated_at,parent_operation_id,root_operation_id,cancel_requested_at,cancel_reason"

func scanBackupOperation(row rowScanner) (domain.BackupOperation, error) {
	var o domain.BackupOperation
	err := row.Scan(&o.ID, &o.PlanID, &o.AgentID, &o.Type, &o.Source, &o.Status, &o.PlanRevision, &o.ConfigHash,
		&o.ScheduledAt, &o.StartedAt, &o.FinishedAt, &o.ExitCode, &o.ErrorCode, &o.SnapshotID,
		&o.FilesDone, &o.TotalFiles, &o.BytesDone, &o.TotalBytes, &o.CreatedAt, &o.UpdatedAt,
		&o.ParentOperationID, &o.RootOperationID, &o.CancelRequestedAt, &o.CancelReason)
	if errors.Is(err, pgx.ErrNoRows) {
		return o, domain.ErrNotFound
	}
	return o, err
}

func (s *Store) BackupOperation(ctx context.Context, id uuid.UUID) (domain.BackupOperation, error) {
	return scanBackupOperation(s.pool.QueryRow(ctx, "select "+backupOperationColumns+" from backup_operations where id=$1", id))
}

func (s *Store) BackupOperations(ctx context.Context, after uuid.UUID, limit int) ([]domain.BackupOperation, error) {
	rows, err := s.pool.Query(ctx, "select "+backupOperationColumns+" from backup_operations where id>$1 order by id desc limit $2", after, limit)
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

// UpsertOperationResult writes the terminal record and indexes its snapshot in
// the same transaction. A duplicate delivery simply rewrites the same terminal
// values; a failed index write rolls the result back for replay.
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
	if _, err = tx.Exec(ctx, `insert into backup_operations(id,plan_id,agent_id,type,source,status,plan_revision,config_hash,
		scheduled_at,started_at,finished_at,exit_code,error_code,snapshot_id,files_done,bytes_done,created_at,updated_at)
		values($1,$2,$3,'BACKUP','SCHEDULED',$4,$5,'',$6,$6,$7,$8,$9,$10,$11,$12,$7,$7)
		on conflict (id) do update set status=excluded.status,finished_at=excluded.finished_at,exit_code=excluded.exit_code,
			error_code=excluded.error_code,snapshot_id=excluded.snapshot_id,files_done=excluded.files_done,
			bytes_done=excluded.bytes_done,updated_at=excluded.updated_at`,
		result.OperationID, result.PlanID, result.AgentID, result.Status, result.PlanRevision, result.ScheduledAt,
		finished, result.ExitCode, result.ErrorCode, result.SnapshotID, result.FilesDone, result.BytesDone); err != nil {
		return persistenceError(err)
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
			result.OperationID, finished, result.FilesDone, result.BytesDone); err != nil {
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

// CancelBackupOperation immediately closes a queued operation and asks the
// Agent to stop a running one. The terminal result remains authoritative.
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
	case "CANCELED", "SUCCEEDED", "SUCCEEDED_WITH_WARNINGS", "FAILED", "MISSED", "CONCURRENCY_SKIPPED":
		return domain.BackupOperation{}, domain.ErrOperationTerminal
	case "CANCEL_REQUESTED":
		return operation, tx.Commit(ctx)
	case "QUEUED":
		operation.Status, operation.FinishedAt = "CANCELED", &now
	default:
		operation.Status = "CANCEL_REQUESTED"
	}
	operation.CancelRequestedAt, operation.CancelReason, operation.UpdatedAt = &now, reason, now
	var finished any
	if operation.FinishedAt != nil {
		finished = *operation.FinishedAt
	}
	if _, err = tx.Exec(ctx, `update backup_operations set status=$2,finished_at=$3,cancel_requested_at=$4,cancel_reason=$5,updated_at=$4
		where id=$1 and finished_at is null`, id, operation.Status, finished, now, reason); err != nil {
		return domain.BackupOperation{}, err
	}
	return operation, tx.Commit(ctx)
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
		where id=$1 and source='MANUAL' and status='RUNNING' and finished_at is null`, id, now)
	return err
}

// NextQueuedManualOperation returns the oldest queued manual job for an Agent.
func (s *Store) NextQueuedManualOperation(ctx context.Context, agentID uuid.UUID) (domain.BackupOperation, bool, error) {
	row := s.pool.QueryRow(ctx, "select "+backupOperationColumns+` from backup_operations
		where agent_id=$1 and source='MANUAL' and status='QUEUED' order by created_at limit 1`, agentID)
	operation, err := scanBackupOperation(row)
	if errors.Is(err, domain.ErrNotFound) {
		return domain.BackupOperation{}, false, nil
	}
	if err != nil {
		return domain.BackupOperation{}, false, err
	}
	return operation, true, nil
}

func (s *Store) MarkOperationDispatched(ctx context.Context, id uuid.UUID) error {
	_, err := s.pool.Exec(ctx, "update backup_operations set status='RUNNING',started_at=coalesce(started_at,now()),updated_at=now() where id=$1 and status='QUEUED'", id)
	return err
}
