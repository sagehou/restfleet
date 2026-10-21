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
	ErrSnapshotAmbiguous     = errors.New("snapshot exists in more than one repository")
	ErrSnapshotUnavailable   = errors.New("snapshot cannot be browsed")
	ErrOperationTerminal     = errors.New("operation is already terminal")
	ErrOperationNotTerminal  = errors.New("operation is not terminal")
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

const RepositoryGatewayObservationLifetime = 10 * time.Minute

// RepositoryGatewayObservation is a verified central read model, not a grant,
// cleanup receipt or backup result. Configuration revisions remain unchanged
// when time-dependent availability changes.
type RepositoryGatewayObservation struct {
	VerifiedAt time.Time
	Authorized bool
}

func (r Repository) WithGatewayObservation(observation RepositoryGatewayObservation, now time.Time) Repository {
	if r.Status != "PROVISIONING" && r.Status != "READY" && r.Status != "DEGRADED" {
		return r
	}
	if observation.VerifiedAt.IsZero() {
		if r.Status == "READY" {
			r.Status = "DEGRADED"
		}
		return r
	}
	r.Status = "DEGRADED"
	if r.InitializedAt != nil && r.AgentCredentialAcceptedAt != nil && observation.Authorized &&
		!observation.VerifiedAt.After(now) && now.Sub(observation.VerifiedAt) < RepositoryGatewayObservationLifetime {
		r.Status = "READY"
	}
	return r
}
