package server

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/sagehou/restfleet/internal/domain"
)

const (
	// dispatchRetryAfter re-sends a JobDispatch whose delivery was not
	// acknowledged. Duplicate delivery is idempotent on the Agent.
	dispatchRetryAfter = 15 * time.Second
	// cancelClosureGrace bounds how long a cancel that never produced a
	// started operation stays pending. It exceeds the JobCancel deadline so a
	// connected Agent always has a chance to report the terminal result.
	cancelClosureGrace = 2 * time.Minute
)

// RecordOperationProgress ingests one throttled progress report. The Agent owns
// scheduling, so progress for an unknown operation creates the row.
func (c *ControlPlane) RecordOperationProgress(ctx context.Context, progress domain.BackupOperationProgress) error {
	if !domain.ValidOperationID(progress.OperationID) ||
		progress.PlanID.Version() != 7 || progress.PlanID.Variant() != uuid.RFC4122 ||
		progress.AgentID.Version() != 7 || progress.AgentID.Variant() != uuid.RFC4122 {
		return domain.ErrNotFound
	}
	return c.store.UpsertOperationProgress(ctx, progress)
}

// RecordOperationResult writes the terminal state and indexes the produced
// snapshot in the same transaction. A failed index write therefore fails the
// whole result so the Agent replays it; it is never silently lost.
func (c *ControlPlane) RecordOperationResult(ctx context.Context, result domain.BackupOperationResult) error {
	if !domain.ValidOperationID(result.OperationID) ||
		result.PlanID.Version() != 7 || result.PlanID.Variant() != uuid.RFC4122 ||
		result.AgentID.Version() != 7 || result.AgentID.Variant() != uuid.RFC4122 {
		return domain.ErrNotFound
	}
	switch result.Status {
	case "SUCCEEDED", "SUCCEEDED_WITH_WARNINGS", "FAILED", "CANCELED", "MISSED", "CONCURRENCY_SKIPPED":
	default:
		return &ValidationError{Field: "status", Code: "INVALID_OPERATION_STATUS"}
	}
	return c.store.UpsertOperationResult(ctx, result)
}

func (c *ControlPlane) Snapshots(ctx context.Context, limit int) ([]domain.Snapshot, error) {
	return c.store.Snapshots(ctx, limit)
}

func (c *ControlPlane) BackupOperations(ctx context.Context, after uuid.UUID, limit int) ([]domain.BackupOperation, error) {
	return c.store.BackupOperations(ctx, after, limit)
}

func (c *ControlPlane) BackupOperation(ctx context.Context, id uuid.UUID) (domain.BackupOperation, error) {
	return c.store.BackupOperation(ctx, id)
}

// FleetBackupCounts aggregates dashboard counters without loading rows.
func (c *ControlPlane) FleetBackupCounts(ctx context.Context) (plans, operations, running, failed24h, unhealthy int64, err error) {
	return c.store.FleetBackupCounts(ctx)
}

// QueueBackupNow creates a manual backup operation for the Agent to pick up on
// its next heartbeat. The plan must already be ACTIVE.
func (c *ControlPlane) QueueBackupNow(ctx context.Context, planID uuid.UUID, actor domain.User, meta RequestMeta) (domain.BackupOperation, error) {
	if actor.Role != domain.RoleAdmin {
		if err := c.RecordDenied(ctx, "PLAN_BACKUP_TRIGGER", "PLAN", "ROLE_DENIED", meta); err != nil {
			return domain.BackupOperation{}, err
		}
		return domain.BackupOperation{}, ErrForbidden
	}
	plan, err := c.store.Plan(ctx, planID)
	if err != nil {
		return domain.BackupOperation{}, err
	}
	if plan.Status != "ACTIVE" {
		return domain.BackupOperation{}, domain.ErrPlanStateInvalid
	}
	id, err := uuid.NewV7()
	if err != nil {
		return domain.BackupOperation{}, err
	}
	now := c.clock().UTC()
	operation := domain.BackupOperation{
		ID: id, PlanID: plan.ID, AgentID: plan.AgentID, Type: "BACKUP", Source: "MANUAL",
		Status: "QUEUED", PlanRevision: plan.DesiredRevision, ConfigHash: plan.ConfigHash,
		ScheduledAt: &now, CreatedAt: now, UpdatedAt: now,
	}
	audit, err := c.userAudit("PLAN_BACKUP_TRIGGER", "PLAN", plan.ID, actor.ID, meta, "QUEUED")
	if err != nil {
		return domain.BackupOperation{}, err
	}
	audit.Changes = json.RawMessage(`{"source":"MANUAL"}`)
	if err := c.store.CreateManualOperation(ctx, operation); err != nil {
		return domain.BackupOperation{}, err
	}
	if err := c.store.RecordAudit(ctx, audit); err != nil {
		return domain.BackupOperation{}, err
	}
	return operation, nil
}

// ClaimQueuedJob atomically claims the next manual job before the Agent stream
// writes JobDispatch. A cancel that commits first makes the claim fail, so a
// canceled QUEUED operation can never start Restic.
func (c *ControlPlane) ClaimQueuedJob(ctx context.Context, agentID uuid.UUID) (domain.BackupOperation, bool, error) {
	return c.store.ClaimManualOperationDispatch(ctx, agentID, c.clock().UTC(), dispatchRetryAfter)
}

func (c *ControlPlane) MarkJobDispatched(ctx context.Context, id uuid.UUID) error {
	return c.store.MarkOperationDispatched(ctx, id)
}

// CancelBackupOperation closes a queued operation immediately or asks the
// Agent to stop a running one. Only a terminal Agent result proves completion.
func (c *ControlPlane) CancelBackupOperation(ctx context.Context, id uuid.UUID, reason string,
	actor domain.User, meta RequestMeta,
) (domain.BackupOperation, error) {
	if actor.Role != domain.RoleAdmin {
		if err := c.RecordDenied(ctx, "BACKUP_OPERATION_CANCEL", "BACKUP_OPERATION", "ROLE_DENIED", meta); err != nil {
			return domain.BackupOperation{}, err
		}
		return domain.BackupOperation{}, ErrForbidden
	}
	reason = strings.TrimSpace(reason)
	if len(reason) > 512 {
		return domain.BackupOperation{}, &ValidationError{Field: "reason", Code: "INVALID_REASON"}
	}
	audit, err := c.userAudit("BACKUP_OPERATION_CANCEL", "BACKUP_OPERATION", id, actor.ID, meta, "CANCEL_REQUESTED")
	if err != nil {
		return domain.BackupOperation{}, err
	}
	operation, err := c.store.CancelBackupOperation(ctx, id, reason, c.clock().UTC())
	if err != nil {
		return domain.BackupOperation{}, err
	}
	if err := c.store.RecordAudit(ctx, audit); err != nil {
		return domain.BackupOperation{}, err
	}
	return operation, nil
}

// RetryBackupOperation creates a new manual run linked to the failed operation.
func (c *ControlPlane) RetryBackupOperation(ctx context.Context, id uuid.UUID,
	actor domain.User, meta RequestMeta,
) (domain.BackupOperation, error) {
	if actor.Role != domain.RoleAdmin {
		if err := c.RecordDenied(ctx, "BACKUP_OPERATION_RETRY", "BACKUP_OPERATION", "ROLE_DENIED", meta); err != nil {
			return domain.BackupOperation{}, err
		}
		return domain.BackupOperation{}, ErrForbidden
	}
	source, err := c.store.BackupOperation(ctx, id)
	if err != nil {
		return domain.BackupOperation{}, err
	}
	if !domain.OperationTerminal(source.Status) {
		return domain.BackupOperation{}, domain.ErrOperationNotTerminal
	}
	plan, err := c.store.Plan(ctx, source.PlanID)
	if err != nil {
		return domain.BackupOperation{}, err
	}
	if plan.Status != "ACTIVE" {
		return domain.BackupOperation{}, domain.ErrPlanStateInvalid
	}
	newID, err := uuid.NewV7()
	if err != nil {
		return domain.BackupOperation{}, err
	}
	now := c.clock().UTC()
	root := source.RootOperationID
	if root == nil {
		root = &source.ID
	}
	operation := domain.BackupOperation{
		ID: newID, PlanID: plan.ID, AgentID: plan.AgentID, Type: "BACKUP", Source: "MANUAL",
		Status: "QUEUED", PlanRevision: plan.DesiredRevision, ConfigHash: plan.ConfigHash,
		ScheduledAt: &now, CreatedAt: now, UpdatedAt: now,
		ParentOperationID: &source.ID, RootOperationID: root,
	}
	audit, err := c.userAudit("BACKUP_OPERATION_RETRY", "BACKUP_OPERATION", newID, actor.ID, meta, "QUEUED")
	if err != nil {
		return domain.BackupOperation{}, err
	}
	audit.Changes = json.RawMessage(`{"parent_operation_id":"` + source.ID.String() + `"}`)
	if err := c.store.CreateManualOperation(ctx, operation); err != nil {
		return domain.BackupOperation{}, err
	}
	if err := c.store.RecordAudit(ctx, audit); err != nil {
		return domain.BackupOperation{}, err
	}
	return operation, nil
}

func (c *ControlPlane) PendingCancellations(ctx context.Context, agentID uuid.UUID) ([]domain.BackupOperation, error) {
	return c.store.PendingCancelOperations(ctx, agentID)
}

// ReconcileOperations closes cancellations that never reached a started
// operation. It is safe to run concurrently with dispatch because both paths
// take a row lock and only terminal transitions are written.
func (c *ControlPlane) ReconcileOperations(ctx context.Context) (int64, error) {
	now := c.clock().UTC()
	return c.store.CloseUnstartedCancellations(ctx, now.Add(-cancelClosureGrace), now)
}

// RunOperationReconciler runs the closure loop until ctx ends. Persistence
// failures are reported through onError and retried instead of aborting the
// server; no operation state is inferred from a failed attempt.
func (c *ControlPlane) RunOperationReconciler(ctx context.Context, interval time.Duration, onError func()) error {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if _, err := c.ReconcileOperations(ctx); err != nil && ctx.Err() == nil && onError != nil {
			onError()
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// RejectManualOperation closes a dispatched manual run the Agent refused.
func (c *ControlPlane) RejectManualOperation(ctx context.Context, id uuid.UUID) error {
	return c.store.RejectManualOperation(ctx, id, c.clock().UTC())
}
