package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/sagehou/restfleet/internal/domain"
)

const backupOperationColumns = "id,plan_id,agent_id,type,source,status,plan_revision,config_hash,scheduled_at,started_at,finished_at,exit_code,error_code,snapshot_id,files_done,total_files,bytes_done,total_bytes,created_at,updated_at,parent_operation_id,root_operation_id,cancel_requested_at,cancel_reason,dispatched_at,attempt,summary_json"

func scanBackupOperation(row rowScanner) (domain.BackupOperation, error) {
	var o domain.BackupOperation
	err := row.Scan(&o.ID, &o.PlanID, &o.AgentID, &o.Type, &o.Source, &o.Status, &o.PlanRevision, &o.ConfigHash,
		&o.ScheduledAt, &o.StartedAt, &o.FinishedAt, &o.ExitCode, &o.ErrorCode, &o.SnapshotID,
		&o.FilesDone, &o.TotalFiles, &o.BytesDone, &o.TotalBytes, &o.CreatedAt, &o.UpdatedAt,
		&o.ParentOperationID, &o.RootOperationID, &o.CancelRequestedAt, &o.CancelReason, &o.DispatchedAt, &o.Attempt, &o.Summary)
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
	if err := lockReportedOperation(ctx, tx, progress.OperationID, progress.AgentID, progress.PlanID, progress.PlanRevision, progress.ParentOperationID, progress.RootOperationID, progress.Attempt); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `insert into backup_operations(id,plan_id,agent_id,type,source,status,plan_revision,config_hash,scheduled_at,started_at,created_at,updated_at,parent_operation_id,root_operation_id,attempt)
		values($1,$2,$3,'BACKUP','SCHEDULED','RUNNING',$4,'',$5,$6,$6,$6,$7,$8,greatest($9,1))
		on conflict (id) do update set status=case when backup_operations.status='CANCEL_REQUESTED' then 'CANCEL_REQUESTED' else 'RUNNING' end,
			updated_at=excluded.updated_at,started_at=coalesce(backup_operations.started_at,excluded.started_at)
		where backup_operations.finished_at is null and backup_operations.agent_id=excluded.agent_id
			and backup_operations.plan_id=excluded.plan_id and backup_operations.plan_revision=excluded.plan_revision
			and (backup_operations.parent_operation_id is not distinct from excluded.parent_operation_id or $9=0)
			and (backup_operations.root_operation_id is not distinct from excluded.root_operation_id or $9=0)
			and (backup_operations.attempt=excluded.attempt or $9=0)`,
		progress.OperationID, progress.PlanID, progress.AgentID, progress.PlanRevision, progress.ScheduledAt, progress.ObservedAt, progress.ParentOperationID, progress.RootOperationID, progress.Attempt)
	if err != nil {
		return persistenceError(err)
	}
	if tag.RowsAffected() == 0 {
		if err := lockReportedOperation(ctx, tx, progress.OperationID, progress.AgentID, progress.PlanID, progress.PlanRevision, progress.ParentOperationID, progress.RootOperationID, progress.Attempt); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	eventTag, err := tx.Exec(ctx, `insert into backup_operation_events(operation_id,sequence,stage,files_done,total_files,bytes_done,total_bytes,observed_at)
		values($1,$2,$3,$4,$5,$6,$7,$8) on conflict (operation_id,sequence) do nothing`,
		progress.OperationID, progress.Sequence, progress.Stage, progress.FilesDone, progress.TotalFiles,
		progress.BytesDone, progress.TotalBytes, progress.ObservedAt)
	if err != nil {
		return err
	}
	if eventTag.RowsAffected() == 0 {
		return tx.Commit(ctx)
	}
	if _, err = tx.Exec(ctx, `update backup_operations set files_done=$2,total_files=$3,bytes_done=$4,total_bytes=$5,updated_at=$6
		where id=$1 and finished_at is null and $7=(select max(sequence) from backup_operation_events where operation_id=$1)`, progress.OperationID, progress.FilesDone, progress.TotalFiles,
		progress.BytesDone, progress.TotalBytes, progress.ObservedAt, progress.Sequence); err != nil {
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
	if err := lockReportedOperation(ctx, tx, result.OperationID, result.AgentID, result.PlanID, result.PlanRevision, result.ParentOperationID, result.RootOperationID, result.Attempt); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `insert into backup_operations(id,plan_id,agent_id,type,source,status,plan_revision,config_hash,
		scheduled_at,started_at,finished_at,exit_code,error_code,snapshot_id,created_at,updated_at,parent_operation_id,root_operation_id,attempt)
		values($1,$2,$3,'BACKUP','SCHEDULED',$4,$5,'',$6,$6,$7,$8,$9,$10,$7,$7,$11,$12,greatest($13,1))
		on conflict (id) do update set status=excluded.status,finished_at=excluded.finished_at,exit_code=excluded.exit_code,
			error_code=excluded.error_code,snapshot_id=excluded.snapshot_id,updated_at=excluded.updated_at
		where backup_operations.finished_at is null and backup_operations.agent_id=excluded.agent_id
			and backup_operations.plan_id=excluded.plan_id and backup_operations.plan_revision=excluded.plan_revision
			and (backup_operations.parent_operation_id is not distinct from excluded.parent_operation_id or $13=0)
			and (backup_operations.root_operation_id is not distinct from excluded.root_operation_id or $13=0)
			and (backup_operations.attempt=excluded.attempt or $13=0)`,
		result.OperationID, result.PlanID, result.AgentID, result.Status, result.PlanRevision, result.ScheduledAt,
		finished, result.ExitCode, result.ErrorCode, result.SnapshotID, result.ParentOperationID, result.RootOperationID, result.Attempt)
	if err != nil {
		return persistenceError(err)
	}
	// A terminal row is authoritative: duplicate or late results may be
	// acknowledged, but must not rewrite it or regress plan health.
	if tag.RowsAffected() == 0 {
		if err := lockReportedOperation(ctx, tx, result.OperationID, result.AgentID, result.PlanID, result.PlanRevision, result.ParentOperationID, result.RootOperationID, result.Attempt); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	if result.Summary != nil {
		if _, err := tx.Exec(ctx, `update backup_operations set summary_json=$2,files_done=$3,total_files=$3,bytes_done=$4,total_bytes=$4 where id=$1`, result.OperationID, result.Summary, result.Summary.TotalFilesProcessed, result.Summary.TotalBytesProcessed); err != nil {
			return err
		}
	}
	health := ""
	success := false
	switch result.Status {
	case "SUCCEEDED", "SUCCEEDED_WITH_WARNINGS":
		health, success = "HEALTHY", true
	case "FAILED", "LOST":
		health = "FAILED"
	case "MISSED":
		health = "OVERDUE"
	}
	if _, err = tx.Exec(ctx, `update plans set
		backup_health=case when status='PAUSED' then 'PAUSED' when $2='' or last_result_at>$5 then backup_health else $2 end,
		last_operation_id=case when last_result_at>$5 then last_operation_id else $3 end,
		last_result_at=greatest(last_result_at,$5),
		last_success_at=case when $4 then greatest(last_success_at,$5) else last_success_at end,
		updated_at=greatest(updated_at,$5) where id=$1`, result.PlanID, health, result.OperationID, success, finished); err != nil {
		return err
	}
	if result.SnapshotID != "" {
		if _, err = tx.Exec(ctx, `insert into snapshots(repository_id,snapshot_id,plan_id,agent_id,host_id,
			operation_id,captured_at,recorded_at,files_new,bytes_added,managed,missing_at)
			values($1,$2,$3,$4,$5,$6,$7,now(),$8,$9,true,null)
			on conflict (repository_id,snapshot_id) do update set
				plan_id=excluded.plan_id,agent_id=excluded.agent_id,host_id=excluded.host_id,
				operation_id=excluded.operation_id,captured_at=excluded.captured_at,recorded_at=excluded.recorded_at,
				files_new=excluded.files_new,bytes_added=excluded.bytes_added,managed=true,missing_at=null`,
			repositoryID, result.SnapshotID, result.PlanID, result.AgentID, hostID,
			result.OperationID, finished, result.FilesNew, result.DataAdded); err != nil {
			return persistenceError(err)
		}
	}
	return tx.Commit(ctx)
}

// An Agent may select its own operation ID, never another plan's existing row.
// The INSERT conflict predicate repeats this check for concurrent first reports.
func lockReportedOperation(ctx context.Context, tx pgx.Tx, id, agentID, planID uuid.UUID, revision int64, parentID, rootID *uuid.UUID, attempt int) error {
	var owner, boundPlan uuid.UUID
	var boundRevision int64
	var parent, root *uuid.UUID
	var boundAttempt int
	err := tx.QueryRow(ctx, "select agent_id,plan_id,plan_revision,parent_operation_id,root_operation_id,attempt from backup_operations where id=$1 for update", id).
		Scan(&owner, &boundPlan, &boundRevision, &parent, &root, &boundAttempt)
	if errors.Is(err, pgx.ErrNoRows) {
		if parentID == nil {
			if rootID != nil || attempt > 1 {
				return domain.ErrPlanBindingInvalid
			}
			return nil
		}
		if rootID == nil || *parentID == id || *rootID == id {
			return domain.ErrPlanBindingInvalid
		}
		previous, err := scanBackupOperation(tx.QueryRow(ctx, "select "+backupOperationColumns+" from backup_operations where id=$1 for share", *parentID))
		if err != nil || previous.AgentID != agentID || previous.PlanID != planID || previous.PlanRevision != revision || previous.Status != "FAILED" || !domain.RetryableBackupError(previous.ErrorCode) || attempt != previous.Attempt+1 || attempt > 10 {
			return domain.ErrPlanBindingInvalid
		}
		expectedRoot := previous.ID
		if previous.RootOperationID != nil {
			expectedRoot = *previous.RootOperationID
		}
		if *rootID != expectedRoot {
			return domain.ErrPlanBindingInvalid
		}
		return nil
	}
	if err != nil {
		return err
	}
	if owner != agentID || boundPlan != planID || boundRevision != revision {
		return domain.ErrPlanBindingInvalid
	}
	if attempt != 0 && (boundAttempt != attempt || !sameOptionalUUID(parent, parentID) || !sameOptionalUUID(root, rootID)) {
		return domain.ErrPlanBindingInvalid
	}
	return nil
}

func sameOptionalUUID(a, b *uuid.UUID) bool {
	return a == nil && b == nil || a != nil && b != nil && *a == *b
}

var _ = time.Now

// FleetBackupCounts feeds the dashboard without loading individual rows.
func (s *Store) FleetBackupCounts(ctx context.Context) (plans, operations, running, failed24h, unhealthy int64, err error) {
	err = s.pool.QueryRow(ctx, `select
		(select count(*) from plans where archived_at is null),
		(select count(*) from backup_operations),
		(select count(*) from backup_operations where status='RUNNING'),
		(select count(*) from backup_operations where status='FAILED' and finished_at > now() - interval '24 hours'),
		(select count(*) from plans where archived_at is null and backup_health in ('FAILED','OVERDUE','NEVER_RUN'))`).
		Scan(&plans, &operations, &running, &failed24h, &unhealthy)
	return
}

func (s *Store) Snapshots(ctx context.Context, limit int) ([]domain.Snapshot, error) {
	rows, err := s.pool.Query(ctx, "select "+snapshotColumns+" from snapshots order by recorded_at desc limit $1", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]domain.Snapshot, 0)
	for rows.Next() {
		item, err := scanSnapshot(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// CreateManualOperation queues a user-triggered backup for one Agent.
func (s *Store) CreateManualOperation(ctx context.Context, operation domain.BackupOperation, request domain.BackupRequestKey, audit domain.AuditEvent) (domain.BackupOperation, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return operation, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	original, found, err := lockBackupRequest(ctx, tx, request)
	if err != nil {
		return operation, err
	}
	if found {
		return original, tx.Commit(ctx)
	}
	if operation.ParentOperationID != nil {
		previous, err := scanBackupOperation(tx.QueryRow(ctx, "select "+backupOperationColumns+" from backup_operations where id=$1 for share", *operation.ParentOperationID))
		if err != nil {
			return operation, err
		}
		if !domain.OperationTerminal(previous.Status) {
			return operation, domain.ErrOperationNotTerminal
		}
		if previous.Attempt >= 10000 {
			return operation, domain.ErrOperationTransition
		}
		operation.PlanID = previous.PlanID
		operation.RootOperationID = previous.RootOperationID
		if operation.RootOperationID == nil {
			operation.RootOperationID = &previous.ID
		}
		operation.Attempt = previous.Attempt + 1
	}
	plan, err := scanPlan(tx.QueryRow(ctx, "select "+planColumns+" from plans where id=$1 and archived_at is null for update", operation.PlanID))
	if err != nil {
		return operation, err
	}
	if plan.Status != "ACTIVE" {
		return operation, domain.ErrPlanStateInvalid
	}
	if err := tx.QueryRow(ctx, "select clock_timestamp()").Scan(&operation.CreatedAt); err != nil {
		return operation, err
	}
	operation.AgentID, operation.PlanRevision, operation.ConfigHash = plan.AgentID, plan.DesiredRevision, plan.ConfigHash
	operation.Type, operation.Source, operation.Status = "BACKUP", "MANUAL", "QUEUED"
	operation.UpdatedAt = operation.CreatedAt
	operation.ScheduledAt = &operation.CreatedAt
	if operation.Attempt == 0 {
		operation.Attempt = 1
	}
	_, err = tx.Exec(ctx, `insert into backup_operations(id,plan_id,agent_id,type,source,status,plan_revision,config_hash,scheduled_at,created_at,updated_at,parent_operation_id,root_operation_id,attempt)
  values($1,$2,$3,'BACKUP','MANUAL','QUEUED',$4,$5,$6,$6,$6,$7,$8,$9)`, operation.ID, operation.PlanID, operation.AgentID, operation.PlanRevision, operation.ConfigHash,
		operation.CreatedAt, operation.ParentOperationID, operation.RootOperationID, operation.Attempt)
	if err != nil {
		return operation, persistenceError(err)
	}
	if err := appendAudit(ctx, tx, audit); err != nil {
		return operation, err
	}
	if err := saveBackupRequest(ctx, tx, request, operation); err != nil {
		return operation, err
	}
	return operation, tx.Commit(ctx)
}

// CancelBackupOperation closes an operation the dispatcher has not claimed and
// asks the Agent to stop one that may already be on the wire. A never-dispatched
// QUEUED row can be closed safely; a claimed row must stay non-terminal until
// the Agent proves it stopped, so a concurrent dispatch can never execute
// unnoticed. The terminal result remains authoritative.
func (s *Store) CancelBackupOperation(ctx context.Context, id uuid.UUID, _ string, now time.Time, request domain.BackupRequestKey, audit domain.AuditEvent) (domain.BackupOperation, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return domain.BackupOperation{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	original, found, err := lockBackupRequest(ctx, tx, request)
	if err != nil {
		return domain.BackupOperation{}, err
	}
	if found {
		return original, tx.Commit(ctx)
	}
	reason := "USER_CANCELED"
	operation, err := scanBackupOperation(tx.QueryRow(ctx, "select "+backupOperationColumns+" from backup_operations where id=$1 for update", id))
	if err != nil {
		return domain.BackupOperation{}, err
	}
	switch operation.Status {
	case "CANCELED", "SUCCEEDED", "SUCCEEDED_WITH_WARNINGS", "FAILED", "LOST", "MISSED", "CONCURRENCY_SKIPPED":
		return domain.BackupOperation{}, domain.ErrOperationTerminal
	case "CANCEL_REQUESTED":
		if err := appendAudit(ctx, tx, audit); err != nil {
			return domain.BackupOperation{}, err
		}
		if err := saveBackupRequest(ctx, tx, request, operation); err != nil {
			return operation, err
		}
		return operation, tx.Commit(ctx)
	case "QUEUED":
		if operation.DispatchedAt == nil {
			operation.Status, operation.FinishedAt, operation.ErrorCode = "CANCELED", &now, "CANCELED"
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
	if err := appendAudit(ctx, tx, audit); err != nil {
		return domain.BackupOperation{}, err
	}
	if err := saveBackupRequest(ctx, tx, request, operation); err != nil {
		return operation, err
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

// ReconcileLostOperations closes operations that were RUNNING under a previous
// Agent boot and were not reported before the grace window expired. A
// boot_changed_at timestamp is written when a new boot id connects, so a
// reconnecting Agent still has time to replay its recovered LOST/CANCELED
// result before the Server draws the same conclusion.
func (s *Store) ReconcileLostOperations(ctx context.Context, bootCutoff, now time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, `update backup_operations o
		set status='LOST',error_code='LOCAL_OPERATION_RECOVERED',finished_at=$2,updated_at=$2
		from agents a
		where o.agent_id=a.id and o.status in ('RUNNING','CANCEL_REQUESTED')
		  and o.finished_at is null
		  and a.boot_changed_at is not null and a.boot_changed_at <= $1
		  and coalesce(o.started_at,o.created_at) < a.boot_changed_at`, bootCutoff, now)
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
func (s *Store) RejectManualOperation(ctx context.Context, agentID, id uuid.UUID, now time.Time) error {
	return s.updateAgentOperation(ctx, agentID, id, `update backup_operations
		set status='FAILED',error_code='AGENT_REJECTED',finished_at=$2,updated_at=$2
		where id=$1 and source='MANUAL' and status in ('QUEUED','RUNNING','CANCEL_REQUESTED') and finished_at is null`, now)
}

// MarkOperationDispatched records JobAccepted. It keeps a canceled row in
// CANCEL_REQUESTED but records started_at, proving the Agent received the job;
// the unstarted-cancellation closure must not close a run that may be active.
func (s *Store) MarkOperationDispatched(ctx context.Context, agentID, id uuid.UUID) error {
	return s.updateAgentOperation(ctx, agentID, id, `update backup_operations
		set status=case when status='QUEUED' then 'RUNNING' else status end,
		    started_at=coalesce(started_at,now()),updated_at=now()
		where id=$1 and status in ('QUEUED','CANCEL_REQUESTED') and finished_at is null`)
}

func (s *Store) updateAgentOperation(ctx context.Context, agentID, id uuid.UUID, query string, values ...any) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var owner uuid.UUID
	if err := tx.QueryRow(ctx, "select agent_id from backup_operations where id=$1 for update", id).Scan(&owner); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrPlanBindingInvalid
		}
		return err
	}
	if owner != agentID {
		return domain.ErrPlanBindingInvalid
	}
	if _, err := tx.Exec(ctx, query, append([]any{id}, values...)...); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
