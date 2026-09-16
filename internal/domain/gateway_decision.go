package domain

import (
	"errors"
	"math"
	"time"

	"github.com/google/uuid"
)

var ErrGatewayDecision = errors.New("gateway authorization decision unavailable or inconsistent")

// GatewayDecisionRequest is central-only. AdmissionID/Owner identify an already
// reserved fence; RuntimeID comes from the trusted process coordinator, not an
// Agent. ID is an idempotency key; ExpectedRevision is zero only for first issue.
type GatewayDecisionRequest struct {
	ID, AdmissionID, Owner, RuntimeID uuid.UUID
	ExpectedRevision                  int64
	Lifetime                          time.Duration
	Revoke                            bool
}

func (r GatewayDecisionRequest) Validate() error {
	for _, id := range []uuid.UUID{r.ID, r.AdmissionID, r.Owner, r.RuntimeID} {
		if id.Version() != 7 || id.Variant() != uuid.RFC4122 {
			return ErrGatewayDecision
		}
	}
	if r.ExpectedRevision < 0 || r.ExpectedRevision == math.MaxInt64 {
		return ErrGatewayDecision
	}
	if r.Revoke {
		if r.Lifetime != 0 {
			return ErrGatewayDecision
		}
	} else if r.Lifetime < time.Second || r.Lifetime > 12*time.Hour || r.Lifetime%time.Second != 0 {
		return ErrGatewayDecision
	}
	return nil
}

// GatewayDecision is a committed metadata decision, never proof of cleanup.
// The immutable admission supplies resource/credential bindings. ExpiresAt is
// zero for an explicit revocation, not for disconnected or expired authority.
type GatewayDecision struct {
	RequestID, RuntimeID uuid.UUID
	Admission            BackupAdmission
	Revision             int64
	RequestedLifetime    time.Duration
	IssuedAt, ExpiresAt  time.Time
	Revoked              bool
}
