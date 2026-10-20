package server

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"
	"github.com/sagehou/restfleet/internal/domain"
)

// RecordOperationProgress ingests one throttled progress report. The Agent owns
// scheduling, so progress for an unknown operation creates the row.
func (c *ControlPlane) RecordOperationProgress(ctx context.Context, progress domain.BackupOperationProgress) error {
	if progress.OperationID.Version() != 7 || progress.PlanID.Version() != 7 || progress.AgentID.Version() != 7 {
		return domain.ErrNotFound
	}
	return c.store.UpsertOperationProgress(ctx, progress)
}

// RecordOperationResult writes the terminal state and indexes the produced
// snapshot in the same transaction. A failed index write therefore fails the
// whole result so the Agent replays it; it is never silently lost.
func (c *ControlPlane) RecordOperationResult(ctx context.Context, result domain.BackupOperationResult) error {
	if result.OperationID.Version() != 7 || result.PlanID.Version() != 7 || result.AgentID.Version() != 7 {
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

func (c *ControlPlane) NextQueuedJob(ctx context.Context, agentID uuid.UUID) (domain.BackupOperation, bool, error) {
	return c.store.NextQueuedManualOperation(ctx, agentID)
}

func (c *ControlPlane) MarkJobDispatched(ctx context.Context, id uuid.UUID) error {
	return c.store.MarkOperationDispatched(ctx, id)
}
