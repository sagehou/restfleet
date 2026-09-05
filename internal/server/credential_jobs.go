package server

import (
	"context"
	"crypto/sha256"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/sagehou/restfleet/internal/domain"
	"github.com/sagehou/restfleet/internal/rclone"
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
	if len(key) < 1 || len(key) > 128 {
		return domain.Operation{}, &ValidationError{Field: "Idempotency-Key", Code: "INVALID_IDEMPOTENCY_KEY"}
	}
	for _, ch := range key {
		if ch < 33 || ch > 126 {
			return domain.Operation{}, &ValidationError{Field: "Idempotency-Key", Code: "INVALID_IDEMPOTENCY_KEY"}
		}
	}
	operationID, err := uuid.NewV7()
	if err != nil {
		return domain.Operation{}, err
	}
	o := domain.Operation{ID: operationID, StorageCredentialID: id, RequestedByUserID: actor.ID}
	audit, err := c.userAudit("STORAGE_CREDENTIAL_TEST", "STORAGE_CREDENTIAL", id, actor.ID, meta, "TEST_QUEUED")
	if err != nil {
		return o, err
	}
	scope := sha256.Sum256([]byte(actor.ID.String() + "\nPOST\n/api/v1/storage-credentials/" + id.String() + "/test"))
	keyHash := sha256.Sum256([]byte(key))
	requestHash := sha256.Sum256(nil) // This endpoint accepts no body.
	return c.store.EnqueueCredentialTest(ctx, o, scope[:], keyHash[:], requestHash[:], audit)
}

// RunCredentialWorker uses PostgreSQL as the authoritative queue. A failed DB
// attempt is retried; shutdown leaves any unfinished lease for crash recovery.
func (c *ControlPlane) RunCredentialWorker(ctx context.Context, onError func()) error {
	if c.runCredentialTest == nil || len(c.masterKey) != 32 {
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
	if c.runCredentialTest == nil || len(c.masterKey) != 32 {
		return false, domain.ErrStorageUnavailable
	}
	job, err := c.store.ClaimCredentialJob(ctx, owner)
	if errors.Is(err, domain.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if job.Credential.Status == "DISABLED" || job.Credential.SecretRevision != job.Operation.SecretRevision {
		return true, c.store.CompleteCredentialJob(ctx, job.ID, owner, "CREDENTIAL_CHANGED")
	}
	if err = c.store.RenewCredentialJob(ctx, job.ID, owner); err != nil {
		return true, err
	}
	envelope, err := c.store.StorageCredentialSecret(ctx, job.Credential.SecretRef)
	if err != nil {
		return true, c.store.CompleteCredentialJob(ctx, job.ID, owner, "SECRET_UNAVAILABLE")
	}
	raw, err := openStorageSecret(c.masterKey, job.Credential, envelope)
	if err != nil {
		return true, c.store.CompleteCredentialJob(ctx, job.ID, owner, "SECRET_UNAVAILABLE")
	}
	defer clear(raw)
	previous, err := rclone.ParseConfig(string(raw), job.Credential.RemoteName)
	if err != nil {
		return true, c.store.CompleteCredentialJob(ctx, job.ID, owner, "CONFIG_UNSAFE")
	}
	workCtx, cancel := context.WithCancel(ctx)
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
				if err := c.store.RenewCredentialJob(workCtx, job.ID, owner); err != nil {
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
	runErr := c.runCredentialTest(workCtx, raw, credential.RemoteName, func(refreshCtx context.Context, nextRaw []byte) error {
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
	})
	cancel()
	renewErr := <-renewed
	if ctx.Err() != nil {
		return true, ctx.Err()
	}
	if renewErr != nil {
		return true, renewErr
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
	return true, c.store.CompleteCredentialJob(ctx, job.ID, owner, code)
}
