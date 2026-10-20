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
	return c.store.UpsertOperationResult(ctx, result)
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
