package server

import (
	"context"

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

// RecordOperationResult writes the terminal state. Duplicate delivery is safe.
func (c *ControlPlane) RecordOperationResult(ctx context.Context, result domain.BackupOperationResult) error {
	if result.OperationID.Version() != 7 || result.PlanID.Version() != 7 || result.AgentID.Version() != 7 {
		return domain.ErrNotFound
	}
	switch result.Status {
	case "SUCCEEDED", "SUCCEEDED_WITH_WARNINGS", "FAILED", "CANCELED", "MISSED", "CONCURRENCY_SKIPPED":
	default:
		return &ValidationError{Field: "status", Code: "INVALID_OPERATION_STATUS"}
	}
	if err := c.store.UpsertOperationResult(ctx, result); err != nil {
		return err
	}
	if result.SnapshotID != "" {
		if plan, err := c.store.Plan(ctx, result.PlanID); err == nil {
			_ = c.store.RecordSnapshot(ctx, domain.Snapshot{
				PlanID: plan.ID, SnapshotID: result.SnapshotID, AgentID: result.AgentID,
				HostID: plan.HostID, RepositoryID: plan.RepositoryID, OperationID: result.OperationID,
				CapturedAt: result.FinishedAt, RecordedAt: c.clock().UTC(),
				FilesNew: result.FilesDone, BytesAdded: result.BytesDone,
			})
		}
	}
	return nil
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
