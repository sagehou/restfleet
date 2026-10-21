package server

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/sagehou/restfleet/internal/domain"
	"github.com/sagehou/restfleet/internal/restic"
)

type SnapshotIndexer func(context.Context, restic.SnapshotIndexRequest, func(context.Context, []byte) error) (domain.SnapshotIndexResult, error)

// RunSnapshotIndexer periodically asks READY repositories for a complete
// `restic snapshots` index. Repository-busy/unavailable outcomes are normal and
// skipped; unexpected persistence errors are reported and retried.
func (c *ControlPlane) RunSnapshotIndexer(ctx context.Context, interval time.Duration, onError func()) error {
	if c.indexSnapshots == nil {
		return domain.ErrStorageUnavailable
	}
	if interval <= 0 {
		interval = 15 * time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := c.indexReadyRepositories(ctx); err != nil && ctx.Err() == nil && onError != nil {
			onError()
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func (c *ControlPlane) indexReadyRepositories(ctx context.Context) error {
	ids, err := c.store.ReadyRepositoryIDs(ctx, 200)
	if err != nil {
		return err
	}
	for _, repositoryID := range ids {
		operationID, err := uuid.NewV7()
		if err != nil {
			return err
		}
		repositoryID := repositoryID
		_, err = c.store.EnqueueSnapshotIndex(ctx, domain.Operation{ID: operationID, RepositoryID: &repositoryID})
		switch {
		case err == nil,
			errors.Is(err, domain.ErrRepositoryBusy),
			errors.Is(err, domain.ErrRepositoryUnavailable),
			errors.Is(err, domain.ErrCredentialDisabled):
			continue
		default:
			return err
		}
	}
	return nil
}

func resolveSnapshotPlan(tags []string) *uuid.UUID {
	managed := false
	var planID *uuid.UUID
	for _, tag := range tags {
		switch {
		case tag == "restfleet:managed":
			managed = true
		case strings.HasPrefix(tag, "restfleet:plan:"):
			parsed, err := uuid.Parse(strings.TrimPrefix(tag, "restfleet:plan:"))
			if err == nil && parsed.Version() == 7 && parsed.Variant() == uuid.RFC4122 {
				candidate := parsed
				if planID == nil {
					planID = &candidate
				} else if *planID != candidate {
					planID = nil
				}
			}
		}
	}
	if !managed {
		return nil
	}
	return planID
}
