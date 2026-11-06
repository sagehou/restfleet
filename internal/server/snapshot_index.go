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
	after := uuid.Nil
	for {
		repositories, err := c.Repositories(ctx, after, 200)
		if err != nil {
			return err
		}
		for _, repository := range repositories {
			after = repository.ID
			if repository.Status != "READY" {
				continue
			}
			operationID, err := uuid.NewV7()
			if err != nil {
				return err
			}
			id := repository.ID
			_, err = c.store.EnqueueSnapshotIndex(ctx, domain.Operation{ID: operationID, RepositoryID: &id}, c.credentialConfigurationHash())
			switch {
			case err == nil, errors.Is(err, domain.ErrRepositoryBusy), errors.Is(err, domain.ErrRepositoryUnavailable), errors.Is(err, domain.ErrCredentialDisabled):
				continue
			default:
				return err
			}
		}
		if len(repositories) < 200 {
			return nil
		}
	}
}

func resolveSnapshotPlan(tags []string) *uuid.UUID {
	managed := false
	ambiguous := false
	var planID *uuid.UUID
	for _, tag := range tags {
		switch {
		case tag == "restfleet:managed":
			managed = true
		case strings.HasPrefix(tag, "restfleet:plan:"):
			parsed, err := uuid.Parse(strings.TrimPrefix(tag, "restfleet:plan:"))
			if err == nil && parsed.Version() == 7 && parsed.Variant() == uuid.RFC4122 && strings.TrimPrefix(tag, "restfleet:plan:") == parsed.String() {
				candidate := parsed
				if planID == nil {
					planID = &candidate
				} else if *planID != candidate {
					ambiguous = true
				}
			} else {
				ambiguous = true
			}
		}
	}
	if !managed || ambiguous {
		return nil
	}
	return planID
}
