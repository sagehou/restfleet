package domain

import (
	"crypto/ed25519"
	"errors"
	"time"

	"github.com/google/uuid"
)

var ErrGatewayGlobalAudit = errors.New("gateway global audit unavailable or inconsistent")

// GatewayAuditBinding identifies only a trusted process audit source. It has
// no Host, Repository, credential, admission or data-plane authority.
type GatewayAuditBinding struct {
	OriginID  uuid.UUID `json:"origin_id"`
	RuntimeID uuid.UUID `json:"runtime_id"`
}

func (b GatewayAuditBinding) Validate() error {
	for _, id := range []uuid.UUID{b.OriginID, b.RuntimeID} {
		if id.Version() != 7 || id.Variant() != uuid.RFC4122 {
			return ErrGatewayGlobalAudit
		}
	}
	return nil
}

type GatewayAuditOrigin struct {
	Binding   GatewayAuditBinding
	PublicKey ed25519.PublicKey
	CreatedAt time.Time
	ClosedAt  *time.Time
}

type GatewayAuditCommit struct {
	Binding                GatewayAuditBinding
	RecordID               uuid.UUID
	Sequence               int64
	PreviousHash, WireHash string
	CreatedAt              time.Time
	Audit                  AuditEvent
}

func GatewayGlobalAudit(event GatewayEvent) (AuditEvent, bool) {
	audit, valid := GatewayAudit(event)
	return audit, valid && event.Binding == (GatewayBinding{}) && !event.Authenticated &&
		(event.Action == "denied" || event.Action == "event_rejected" || event.Action == "channel_denied")
}
