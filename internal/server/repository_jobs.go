package server

import (
	"context"
	"crypto/sha256"
	"errors"

	"github.com/google/uuid"
	"github.com/sagehou/restfleet/internal/domain"
	"github.com/sagehou/restfleet/internal/restic"
)

type RepositoryInitializer func(context.Context, restic.ProvisionRequest, func(context.Context, []byte) error) (restic.RepositoryInfo, error)

func (c *ControlPlane) InitializeRepository(ctx context.Context, id uuid.UUID, key string, actor domain.User, meta RequestMeta) (domain.Operation, error) {
	if err := c.requireStorageAdmin(ctx, actor, meta); err != nil {
		return domain.Operation{}, err
	}
	if c.initializeRepository == nil || len(c.masterKey) != 32 {
		return domain.Operation{}, domain.ErrStorageUnavailable
	}
	if err := validateOperationKey(key); err != nil {
		return domain.Operation{}, err
	}
	operationID, err := uuid.NewV7()
	if err != nil {
		return domain.Operation{}, err
	}
	o := domain.Operation{ID: operationID, RepositoryID: &id, RequestedByUserID: actor.ID}
	audit, err := c.userAudit("REPOSITORY_INITIALIZE", "REPOSITORY", id, actor.ID, meta, "INITIALIZE_QUEUED")
	if err != nil {
		return o, err
	}
	scope := sha256.Sum256([]byte(actor.ID.String() + "\nPOST\n/api/v1/repositories/" + id.String() + "/initialize"))
	keyHash := sha256.Sum256([]byte(key))
	request := sha256.Sum256(nil)
	return c.store.EnqueueStorageOperation(ctx, o, scope[:], keyHash[:], request[:], audit)
}

func initializeErrorCode(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, context.DeadlineExceeded):
		return "INITIALIZE_TIMED_OUT"
	case errors.Is(err, restic.ErrRepositoryLocked):
		return "REPOSITORY_LOCKED"
	case errors.Is(err, restic.ErrRepositoryMatch), errors.Is(err, restic.ErrRepositoryAbsent):
		return "REPOSITORY_MISMATCH"
	case errors.Is(err, restic.ErrRepositoryInUse):
		return "REPOSITORY_NOT_EMPTY"
	case errors.Is(err, restic.ErrWrongPassword):
		return "PASSWORD_REJECTED"
	default:
		return "INITIALIZE_FAILED"
	}
}
