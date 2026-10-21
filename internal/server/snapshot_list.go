package server

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/sagehou/restfleet/internal/domain"
	"github.com/sagehou/restfleet/internal/restic"
)

// SnapshotLister is the central read-only repository adapter. It is a job
// dependency, not an HTTP path: refresh callbacks and leases stay in the worker.
type SnapshotLister func(context.Context, restic.ListRequest, func(context.Context, []byte) error) (restic.ListResult, error)

// SnapshotEntries returns a fresh cached page or queues one bounded scan. The
// bool reports that a scan is pending; it is not an error.
func (c *ControlPlane) SnapshotEntries(ctx context.Context, snapshotID string,
	repositoryID *uuid.UUID, rawPath string, requestedBy uuid.UUID,
) (domain.SnapshotListResult, bool, error) {
	if c.listSnapshot == nil || len(c.masterKey) != 32 {
		return domain.SnapshotListResult{}, false, domain.ErrStorageUnavailable
	}
	if len(snapshotID) != 64 {
		return domain.SnapshotListResult{}, false, domain.ErrNotFound
	}
	path, err := restic.NormalizeListPath(rawPath)
	if err != nil {
		return domain.SnapshotListResult{}, false, &ValidationError{Field: "path", Code: "INVALID_PATH"}
	}
	snapshots, err := c.store.SnapshotsByID(ctx, snapshotID)
	if err != nil {
		return domain.SnapshotListResult{}, false, err
	}
	if len(snapshots) == 0 {
		return domain.SnapshotListResult{}, false, domain.ErrNotFound
	}
	repositories := make(map[uuid.UUID]struct{}, len(snapshots))
	for _, snapshot := range snapshots {
		repositories[snapshot.RepositoryID] = struct{}{}
	}
	var resolved uuid.UUID
	if repositoryID != nil {
		if _, ok := repositories[*repositoryID]; !ok {
			return domain.SnapshotListResult{}, false, domain.ErrNotFound
		}
		resolved = *repositoryID
	} else if len(repositories) == 1 {
		for id := range repositories {
			resolved = id
		}
	} else {
		return domain.SnapshotListResult{}, false, domain.ErrSnapshotAmbiguous
	}
	repository, err := c.store.Repository(ctx, resolved)
	if err != nil {
		return domain.SnapshotListResult{}, false, err
	}
	if repository.ResticID == "" || repository.InitializedAt == nil {
		return domain.SnapshotListResult{}, false, domain.ErrSnapshotUnavailable
	}
	now := c.clock().UTC()
	result, fresh, err := c.store.SnapshotListResult(ctx, resolved, snapshotID, path, now)
	if err != nil {
		return domain.SnapshotListResult{}, false, err
	}
	if fresh {
		return result, false, nil
	}
	active, scanning, err := c.store.ActiveSnapshotListOperation(ctx, resolved)
	if err != nil {
		return domain.SnapshotListResult{}, false, err
	}
	if scanning {
		return domain.SnapshotListResult{
			OperationID: active.ID, RepositoryID: resolved, SnapshotID: snapshotID, Path: path,
		}, true, nil
	}
	operationID, err := uuid.NewV7()
	if err != nil {
		return domain.SnapshotListResult{}, false, err
	}
	operation, err := c.store.EnqueueSnapshotList(ctx, domain.Operation{
		ID: operationID, RepositoryID: &resolved, RequestedByUserID: &requestedBy,
	}, snapshotID, path)
	if errors.Is(err, domain.ErrRepositoryBusy) {
		return domain.SnapshotListResult{}, false, domain.ErrRepositoryBusy
	}
	if err != nil {
		return domain.SnapshotListResult{}, false, err
	}
	return domain.SnapshotListResult{
		OperationID: operation.ID, RepositoryID: resolved, SnapshotID: snapshotID, Path: path,
	}, true, nil
}
