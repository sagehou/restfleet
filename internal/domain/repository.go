package domain

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

var (
	ErrRepositoryUnavailable = errors.New("repository cannot be initialized")
	ErrRepositoryBusy        = errors.New("repository or storage credential is busy")
	ErrHostRepositoryExists  = errors.New("host already owns a repository")
	ErrSharedRepository      = errors.New("shared repositories are not supported")
	ErrHostUnavailable       = errors.New("host is disabled or revoked")
)

// Repository contains metadata and secret references, never plaintext credentials.
type Repository struct {
	ID, HostID, StorageCredentialID                       uuid.UUID
	Name, Status, BackendPath                             string
	GatewayID                                             uuid.UUID
	GatewaySecretRef, ResticSecretRef                     uuid.UUID
	GatewaySecretRevision, ResticSecretRevision, Revision int64
	FormatVersion                                         *int
	ResticID                                              string
	InitializedAt                                         *time.Time
	AgentCredentialRevision                               *int64
	AgentCredentialAcceptedAt                             *time.Time
	LastInitializeOperationID                             *uuid.UUID
	CreatedAt, UpdatedAt                                  time.Time
}
