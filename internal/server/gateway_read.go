package server

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/sagehou/restfleet/internal/domain"
	"github.com/sagehou/restfleet/internal/restic"
)

func (c *ControlPlane) processGatewayReadJob(ctx context.Context, job domain.CredentialJob, owner uuid.UUID) error {
	complete := func(code string, index domain.SnapshotIndexResult, list domain.SnapshotListResult) error {
		if job.Operation.Type == "SNAPSHOT_INDEX" {
			return c.store.FinishSnapshotIndexJob(ctx, job.ID, owner, code, index)
		}
		return c.store.FinishSnapshotListJob(ctx, job.ID, owner, code, list)
	}
	if job.Repository == nil || job.GatewayRead == nil || job.GatewayRead.ConfigurationHash != c.credentialConfigurationHash() || c.enrollment.CA == nil || c.gatewayPublicURL == "" {
		return complete("CONFIG_UNSAFE", domain.SnapshotIndexResult{}, domain.SnapshotListResult{})
	}
	if err := c.store.RenewCredentialJob(ctx, job.ID, owner); err != nil {
		if errors.Is(err, domain.ErrRepositoryBusy) || errors.Is(err, domain.ErrRepositoryUnavailable) {
			return complete("REPOSITORY_UNAVAILABLE", domain.SnapshotIndexResult{}, domain.SnapshotListResult{})
		}
		return err
	}
	work, cancel := context.WithTimeout(ctx, 5*time.Minute)
	renewed := make(chan error, 1)
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-work.Done():
				renewed <- nil
				return
			case <-ticker.C:
				check, stop := context.WithTimeout(work, 3*time.Second)
				err := c.store.RenewCredentialJob(check, job.ID, owner)
				stop()
				if err != nil {
					cancel()
					renewed <- err
					return
				}
			}
		}
	}()
	var index domain.SnapshotIndexResult
	var list domain.SnapshotListResult
	var runErr error
	envelope, err := c.store.RepositoryResticSecret(work, job.Repository.ID)
	var password []byte
	if err == nil {
		password, err = openRepositorySecret(c.masterKey, *job.Repository, "RESTIC_KEY", envelope)
	}
	if err != nil {
		runErr = domain.ErrStorageUnavailable
	} else {
		certificate, key, err := c.enrollment.CA.IssueGatewayReader(*job.GatewayRead, c.clock().UTC())
		if err != nil {
			runErr = domain.ErrStorageUnavailable
		} else {
			access := &restic.GatewayReadAccess{Origin: c.gatewayPublicURL, Binding: *job.GatewayRead, CertificatePEM: certificate, PrivateKeyPEM: key, CABundlePEM: c.enrollment.ServerCABundlePEM}
			// Neither a cloud config nor a refresh callback is supplied in this mode.
			if job.Operation.Type == "SNAPSHOT_INDEX" && c.indexSnapshots != nil {
				index, runErr = c.indexSnapshots(work, restic.SnapshotIndexRequest{Gateway: access, GatewayID: job.Repository.GatewayID, RepositoryID: job.Repository.ID, Password: password}, nil)
				for i := range index.Entries {
					index.Entries[i].PlanID = resolveSnapshotPlan(index.Entries[i].Tags)
				}
			} else if job.Operation.Type == "SNAPSHOT_LIST" && c.listSnapshot != nil {
				var payload struct {
					SnapshotID string `json:"snapshot_id"`
					Path       string `json:"path"`
					Cursor     string `json:"cursor"`
				}
				if json.Unmarshal(job.Payload, &payload) != nil {
					runErr = restic.ErrInvalidListRequest
				} else {
					var result restic.ListResult
					result, runErr = c.listSnapshot(work, restic.ListRequest{Gateway: access, GatewayID: job.Repository.GatewayID, RepositoryID: job.Repository.ID, Password: password, SnapshotID: payload.SnapshotID, Path: payload.Path, After: payload.Cursor}, nil)
					list = domain.SnapshotListResult{SnapshotID: payload.SnapshotID, TreeID: result.TreeID, Path: payload.Path, Truncated: result.Truncated, NextCursor: result.NextCursor, Scanned: result.Scanned, Entries: make([]domain.SnapshotEntry, 0, len(result.Entries))}
					for _, node := range result.Entries {
						list.Entries = append(list.Entries, domain.SnapshotEntry{Name: node.Name, Path: node.Path, Type: node.Type, Size: node.Size, Mode: node.Mode, UID: node.UID, GID: node.GID, ModTime: node.ModTime})
					}
				}
			} else {
				runErr = restic.ErrProcessFailed
			}
			clear(key)
		}
	}
	clear(password)
	if work.Err() != nil && runErr == nil {
		runErr = work.Err()
	}
	cancel()
	renewErr := <-renewed
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if renewErr != nil {
		if errors.Is(renewErr, domain.ErrRepositoryBusy) || errors.Is(renewErr, domain.ErrRepositoryUnavailable) {
			return complete("REPOSITORY_UNAVAILABLE", domain.SnapshotIndexResult{}, domain.SnapshotListResult{})
		}
		return renewErr
	}
	code := snapshotIndexErrorCode(runErr)
	if job.Operation.Type == "SNAPSHOT_LIST" {
		code = snapshotListErrorCode(runErr)
	}
	return complete(code, index, list)
}
