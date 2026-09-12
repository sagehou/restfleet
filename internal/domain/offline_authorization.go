package domain

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

var (
	ErrOfflineAuthorization = errors.New("offline authorization unavailable")
	ErrAuthorizationExpired = errors.New("offline authorization has expired")
	ErrAuthorizationRevoked = errors.New("offline authorization has been revoked")
	ErrAntiRollback         = errors.New("offline authorization anti-rollback failure")
)

// OfflineAuthorizationRequest is central-only. The trusted caller allocates
// ID and Owner as durable UUIDv7 identities. GatewayInstanceID identifies the
// specific runtime process that will use the authorization.
type OfflineAuthorizationRequest struct {
	ID, Owner           uuid.UUID
	GatewayInstanceID   uuid.UUID
	AgentID             uuid.UUID
	DeliveryID          uuid.UUID
	ConfigurationHash   string
	Lifetime            time.Duration
}

func (r OfflineAuthorizationRequest) Validate() error {
	for _, id := range []uuid.UUID{r.ID, r.Owner, r.GatewayInstanceID, r.AgentID, r.DeliveryID} {
		if id.Version() != 7 || id.Variant() != uuid.RFC4122 {
			return ErrOfflineAuthorization
		}
	}
	if len(r.ConfigurationHash) != 64 {
		return ErrOfflineAuthorization
	}
	for _, c := range r.ConfigurationHash {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return ErrOfflineAuthorization
		}
	}
	if r.Lifetime < time.Minute || r.Lifetime > 12*time.Hour || r.Lifetime%time.Second != 0 {
		return ErrOfflineAuthorization
	}
	return nil
}

// OfflineAuthorization is a bounded offline grant for an independent Gateway.
// It binds to a specific runtime instance, repository, credential and delivery
// state. The monotonic AuthorizationSequence prevents replay of older grants.
//
// Authorization does NOT prove the backend is READY, the Agent has accepted,
// or that the central maintenance/credential-test writers are excluded. The
// Gateway MUST still acquire a backup admission before each actual session.
type OfflineAuthorization struct {
	ID                    uuid.UUID
	Owner                 uuid.UUID
	GatewayInstanceID     uuid.UUID
	AgentID               uuid.UUID
	HostID                uuid.UUID
	RepositoryID          uuid.UUID
	GatewayID             uuid.UUID
	StorageCredentialID   uuid.UUID
	DeliveryID            uuid.UUID
	ConfigurationHash     string
	AuthorizedAt          time.Time
	ExpiresAt             time.Time
	AuthorizationSequence int64
	RenewedAt             *time.Time
	RevokedAt             *time.Time
	DisabledAt            *time.Time
	CreatedAt             time.Time
}

// Active reports whether the authorization is currently usable: not revoked,
// not disabled and not expired. Expiration is measured against the provided
// clock so the Gateway can use its own monotonic time.
func (a OfflineAuthorization) Active(now time.Time) bool {
	return a.RevokedAt == nil && a.DisabledAt == nil && now.Before(a.ExpiresAt)
}

// RemainingAuthorization returns how much time is left on this grant. Returns
// zero if already expired or revoked/disabled.
func (a OfflineAuthorization) RemainingAuthorization(now time.Time) time.Duration {
	if !a.Active(now) {
		return 0
	}
	return a.ExpiresAt.Sub(now)
}

// OfflineRenewalRequest extends an existing authorization. The caller MUST
// supply the current AuthorizationSequence for anti-rollback verification.
type OfflineRenewalRequest struct {
	AuthorizationID      uuid.UUID
	Owner                uuid.UUID
	CurrentSequence      int64
	NewExpiresAt         time.Time
	ConfigurationHash    string
}

func (r OfflineRenewalRequest) Validate(maxLifetime time.Duration) error {
	if r.AuthorizationID.Version() != 7 || r.AuthorizationID.Variant() != uuid.RFC4122 {
		return ErrOfflineAuthorization
	}
	if r.Owner.Version() != 7 || r.Owner.Variant() != uuid.RFC4122 {
		return ErrOfflineAuthorization
	}
	if r.CurrentSequence < 1 {
		return ErrAntiRollback
	}
	if len(r.ConfigurationHash) != 64 {
		return ErrOfflineAuthorization
	}
	if maxLifetime > 0 && time.Until(r.NewExpiresAt) > maxLifetime {
		return ErrOfflineAuthorization
	}
	return nil
}
