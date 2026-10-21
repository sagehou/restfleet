package server

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/sagehou/restfleet/internal/domain"
	"github.com/sagehou/restfleet/internal/rclone"
	"github.com/sagehou/restfleet/internal/restic"
)

type CredentialTestRunner func(context.Context, []byte, string, func(context.Context, []byte) error) error

func (c *ControlPlane) Operation(ctx context.Context, id uuid.UUID) (domain.Operation, error) {
	return c.store.Operation(ctx, id)
}

func (c *ControlPlane) TestStorageCredential(ctx context.Context, id uuid.UUID, key string, actor domain.User, meta RequestMeta) (domain.Operation, error) {
	if err := c.requireStorageAdmin(ctx, actor, meta); err != nil {
		return domain.Operation{}, err
	}
	if c.runCredentialTest == nil {
		return domain.Operation{}, domain.ErrStorageUnavailable
	}
	if err := validateOperationKey(key); err != nil {
		return domain.Operation{}, err
	}
	operationID, err := uuid.NewV7()
	if err != nil {
		return domain.Operation{}, err
	}
	o := domain.Operation{ID: operationID, StorageCredentialID: id, RequestedByUserID: &actor.ID}
	audit, err := c.userAudit("STORAGE_CREDENTIAL_TEST", "STORAGE_CREDENTIAL", id, actor.ID, meta, "TEST_QUEUED")
	if err != nil {
		return o, err
	}
	scope := sha256.Sum256([]byte(actor.ID.String() + "\nPOST\n/api/v1/storage-credentials/" + id.String() + "/test"))
	keyHash := sha256.Sum256([]byte(key))
	requestHash := sha256.Sum256(nil) // This endpoint accepts no body.
	return c.store.EnqueueStorageOperation(ctx, o, scope[:], keyHash[:], requestHash[:], audit)
}

// ponytail: one central worker handles both storage tests and initialization;
// per-credential parallelism can be added only with runtime ownership fencing.
// RunCredentialWorker uses PostgreSQL as the authoritative queue. A failed DB
// attempt is retried; shutdown leaves any unfinished lease for crash recovery.
func (c *ControlPlane) RunCredentialWorker(ctx context.Context, onError func()) error {
	if (c.runCredentialTest == nil && c.initializeRepository == nil && c.listSnapshot == nil && c.indexSnapshots == nil) || len(c.masterKey) != 32 {
		return domain.ErrStorageUnavailable
	}
	owner, err := uuid.NewV7()
	if err != nil {
		return err
	}
	for ctx.Err() == nil {
		worked, err := c.ProcessCredentialJob(ctx, owner)
		if err != nil && ctx.Err() == nil && onError != nil {
			onError()
		}
		if worked && err == nil {
			continue
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
	}
	return nil
}

// ProcessCredentialJob is also the integration-test entry point for restart,
// duplicate delivery and lease fencing without a second in-memory queue.
func (c *ControlPlane) ProcessCredentialJob(ctx context.Context, owner uuid.UUID) (bool, error) {
	if (c.runCredentialTest == nil && c.initializeRepository == nil && c.listSnapshot == nil && c.indexSnapshots == nil) || len(c.masterKey) != 32 {
		return false, domain.ErrStorageUnavailable
	}
	job, err := c.store.ClaimCredentialJob(ctx, owner)
	if errors.Is(err, domain.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var info restic.RepositoryInfo
	complete := func(code string) error {
		if job.Operation.Type == "SNAPSHOT_LIST" {
			return c.store.FinishSnapshotListJob(ctx, job.ID, owner, code, domain.SnapshotListResult{})
		}
		if job.Operation.Type == "SNAPSHOT_INDEX" {
			return c.store.FinishSnapshotIndexJob(ctx, job.ID, owner, code, domain.SnapshotIndexResult{})
		}
		if job.Repository != nil {
			return c.store.CompleteRepositoryJob(ctx, job.ID, owner, code, info.ID, info.FormatVersion)
		}
		return c.store.CompleteCredentialJob(ctx, job.ID, owner, code)
	}
	if job.Repository != nil && !job.RepositoryAvailable {
		return true, complete("REPOSITORY_UNAVAILABLE")
	}
	if job.Credential.Status == "DISABLED" || job.Credential.SecretRevision != job.Operation.SecretRevision {
		return true, complete("CREDENTIAL_CHANGED")
	}
	if err = c.store.RenewCredentialJob(ctx, job.ID, owner); err != nil {
		return true, err
	}
	envelope, err := c.store.StorageCredentialSecret(ctx, job.Credential.SecretRef)
	if err != nil {
		return true, complete("SECRET_UNAVAILABLE")
	}
	raw, err := openStorageSecret(c.masterKey, job.Credential, envelope)
	if err != nil {
		return true, complete("SECRET_UNAVAILABLE")
	}
	defer clear(raw)
	previous, err := rclone.ParseConfig(string(raw), job.Credential.RemoteName)
	if err != nil || storageProvider(previous) != job.Credential.Provider {
		return true, complete("CONFIG_UNSAFE")
	}
	workCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	renewed := make(chan error, 1)
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-workCtx.Done():
				renewed <- nil
				return
			case <-ticker.C:
				renewCtx, stopRenew := context.WithTimeout(workCtx, 3*time.Second)
				err := c.store.RenewCredentialJob(renewCtx, job.ID, owner)
				stopRenew()
				if err != nil {
					if workCtx.Err() != nil {
						renewed <- nil
						return
					}
					cancel()
					renewed <- err
					return
				}
			}
		}
	}()
	credential := job.Credential
	persist := func(refreshCtx context.Context, nextRaw []byte) error {
		next, err := rclone.ParseConfig(string(nextRaw), credential.RemoteName)
		if err != nil || !previous.SameExceptToken(next) {
			return rclone.ErrConfigChanged
		}
		nextCredential := credential
		nextCredential.SecretRevision++
		nextCredential.UpdatedAt = c.clock().UTC()
		sealed, err := c.sealStorageConfig(nextCredential, next)
		if err != nil {
			return err
		}
		saved, err := c.store.RefreshCredentialJob(refreshCtx, job.ID, owner, credential.SecretRevision, sealed)
		if err != nil {
			return err
		}
		credential, previous = saved, next
		return nil
	}
	var runErr error
	var snapshotResult domain.SnapshotListResult
	var snapshotIndexResult domain.SnapshotIndexResult
	if job.Repository == nil {
		if c.runCredentialTest == nil {
			runErr = rclone.ErrCommandFailed
		} else {
			runErr = c.runCredentialTest(workCtx, raw, credential.RemoteName, persist)
		}
	} else if job.Operation.Type == "SNAPSHOT_LIST" {
		if c.listSnapshot == nil {
			runErr = restic.ErrProcessFailed
		} else {
			var payload struct {
				SnapshotID string `json:"snapshot_id"`
				Path       string `json:"path"`
				Cursor     string `json:"cursor"`
			}
			if json.Unmarshal(job.Payload, &payload) != nil {
				runErr = restic.ErrInvalidListRequest
			} else if e, secretErr := c.store.RepositoryResticSecret(workCtx, job.Repository.ID); secretErr != nil {
				runErr = domain.ErrStorageUnavailable
			} else if password, secretErr := openRepositorySecret(c.masterKey, *job.Repository, "RESTIC_KEY", e); secretErr != nil {
				runErr = domain.ErrStorageUnavailable
			} else {
				listCtx, stop := context.WithTimeout(workCtx, time.Minute)
				list, listErr := c.listSnapshot(listCtx, restic.ListRequest{
					Config: raw, Remote: credential.RemoteName,
					GatewayID: job.Repository.GatewayID, RepositoryID: job.Repository.ID,
					Password: password, SnapshotID: payload.SnapshotID, Path: payload.Path, After: payload.Cursor,
				}, persist)
				stop()
				clear(password)
				runErr = listErr
				if listErr == nil {
					snapshotResult = domain.SnapshotListResult{
						SnapshotID: payload.SnapshotID, TreeID: list.TreeID, Path: payload.Path,
						Truncated: list.Truncated, NextCursor: list.NextCursor, Scanned: list.Scanned,
						Entries: make([]domain.SnapshotEntry, 0, len(list.Entries)),
					}
					for _, node := range list.Entries {
						snapshotResult.Entries = append(snapshotResult.Entries, domain.SnapshotEntry{
							Name: node.Name, Path: node.Path, Type: node.Type, Size: node.Size,
							Mode: node.Mode, UID: node.UID, GID: node.GID, ModTime: node.ModTime,
						})
					}
				}
			}
		}
	} else if job.Operation.Type == "SNAPSHOT_INDEX" {
		if c.indexSnapshots == nil {
			runErr = restic.ErrProcessFailed
		} else if e, secretErr := c.store.RepositoryResticSecret(workCtx, job.Repository.ID); secretErr != nil {
			runErr = domain.ErrStorageUnavailable
		} else if password, secretErr := openRepositorySecret(c.masterKey, *job.Repository, "RESTIC_KEY", e); secretErr != nil {
			runErr = domain.ErrStorageUnavailable
		} else {
			indexCtx, stop := context.WithTimeout(workCtx, 5*time.Minute)
			indexed, indexErr := c.indexSnapshots(indexCtx, restic.SnapshotIndexRequest{
				Config: raw, Remote: credential.RemoteName,
				GatewayID: job.Repository.GatewayID, RepositoryID: job.Repository.ID,
				Password: password,
			}, persist)
			stop()
			clear(password)
			runErr = indexErr
			if indexErr == nil {
				for i := range indexed.Entries {
					indexed.Entries[i].PlanID = resolveSnapshotPlan(indexed.Entries[i].Tags)
				}
				snapshotIndexResult = indexed
			}
		}
	} else if c.initializeRepository == nil {
		runErr = restic.ErrProcessFailed
	} else {
		e, secretErr := c.store.RepositoryResticSecret(workCtx, job.Repository.ID)
		var password []byte
		if secretErr == nil {
			password, secretErr = openRepositorySecret(c.masterKey, *job.Repository, "RESTIC_KEY", e)
		}
		if secretErr != nil {
			runErr = domain.ErrStorageUnavailable
		} else {
			info, runErr = c.initializeRepository(workCtx, restic.ProvisionRequest{Config: raw, Remote: credential.RemoteName,
				GatewayID: job.Repository.GatewayID, RepositoryID: job.Repository.ID, Password: password, ExpectedID: job.Repository.ResticID}, persist)
			clear(password)
			if runErr == nil && !domain.ValidInitializedRepository(info.ID, info.FormatVersion) {
				runErr = restic.ErrInvalidOutput
			}
		}
	}
	if workCtx.Err() != nil && runErr == nil {
		runErr = workCtx.Err()
	}
	cancel()
	renewErr := <-renewed
	if ctx.Err() != nil {
		return true, ctx.Err()
	}
	if renewErr != nil {
		return true, renewErr
	}
	if job.Operation.Type == "SNAPSHOT_LIST" {
		return true, c.store.FinishSnapshotListJob(ctx, job.ID, owner, snapshotListErrorCode(runErr), snapshotResult)
	}
	if job.Operation.Type == "SNAPSHOT_INDEX" {
		return true, c.store.FinishSnapshotIndexJob(ctx, job.ID, owner, snapshotIndexErrorCode(runErr), snapshotIndexResult)
	}
	code := ""
	switch {
	case runErr == nil:
	case errors.Is(runErr, context.DeadlineExceeded):
		code = "TEST_TIMED_OUT"
	case errors.Is(runErr, rclone.ErrRefreshPersist):
		code = "REFRESH_FAILED"
	case errors.Is(runErr, rclone.ErrInvalidConfig), errors.Is(runErr, rclone.ErrUnsafeRuntime), errors.Is(runErr, rclone.ErrConfigChanged):
		code = "CONFIG_UNSAFE"
	default:
		code = "CONNECTION_FAILED"
	}
	if job.Repository != nil {
		switch {
		case errors.Is(runErr, domain.ErrStorageUnavailable):
			code = "SECRET_UNAVAILABLE"
		case code == "CONFIG_UNSAFE" || code == "REFRESH_FAILED":
		default:
			code = initializeErrorCode(runErr)
		}
	}
	return true, complete(code)
}

func snapshotListErrorCode(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, context.DeadlineExceeded):
		return "SCAN_TIMED_OUT"
	case errors.Is(err, restic.ErrRepositoryLocked):
		return "REPOSITORY_LOCKED"
	case errors.Is(err, restic.ErrWrongPassword):
		return "PASSWORD_REJECTED"
	case errors.Is(err, restic.ErrRepositoryAbsent):
		return "REPOSITORY_UNAVAILABLE"
	case errors.Is(err, domain.ErrStorageUnavailable):
		return "SECRET_UNAVAILABLE"
	case errors.Is(err, restic.ErrInvalidListRequest):
		return "PATH_INVALID"
	case errors.Is(err, rclone.ErrRefreshPersist):
		return "REFRESH_FAILED"
	case errors.Is(err, rclone.ErrInvalidConfig), errors.Is(err, rclone.ErrUnsafeRuntime), errors.Is(err, rclone.ErrConfigChanged):
		return "CONFIG_UNSAFE"
	default:
		return "SCAN_FAILED"
	}
}

func snapshotIndexErrorCode(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, context.DeadlineExceeded):
		return "INDEX_TIMED_OUT"
	case errors.Is(err, restic.ErrRepositoryLocked):
		return "REPOSITORY_LOCKED"
	case errors.Is(err, restic.ErrWrongPassword):
		return "PASSWORD_REJECTED"
	case errors.Is(err, restic.ErrRepositoryAbsent):
		return "REPOSITORY_UNAVAILABLE"
	case errors.Is(err, domain.ErrStorageUnavailable):
		return "SECRET_UNAVAILABLE"
	case errors.Is(err, rclone.ErrRefreshPersist):
		return "REFRESH_FAILED"
	case errors.Is(err, rclone.ErrInvalidConfig), errors.Is(err, rclone.ErrUnsafeRuntime), errors.Is(err, rclone.ErrConfigChanged):
		return "CONFIG_UNSAFE"
	default:
		return "INDEX_FAILED"
	}
}

func validateOperationKey(key string) error {
	if len(key) < 1 || len(key) > 128 {
		return &ValidationError{Field: "Idempotency-Key", Code: "INVALID_IDEMPOTENCY_KEY"}
	}
	for _, ch := range key {
		if ch < 33 || ch > 126 {
			return &ValidationError{Field: "Idempotency-Key", Code: "INVALID_IDEMPOTENCY_KEY"}
		}
	}
	return nil
}
