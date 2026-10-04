package domain

import (
	"crypto/ed25519"
	"errors"
	"time"

	"github.com/google/uuid"
)

var ErrGatewayPending = errors.New("gateway pending replay unavailable or inconsistent")

// GatewayPendingOrigin is registered by a trusted CENTRAL coordinator, never
// chosen by a replay caller. It is immutable except explicit central sealing.
type GatewayPendingOrigin struct {
	Admission             BackupAdmission
	RuntimeID             uuid.UUID
	PublicKey             ed25519.PublicKey
	InitialSecretRevision int64
	ClosedAt              *time.Time
}

// GatewayPendingCommit contains safe metadata only. Token plaintext is handled
// solely in the central validator while the database owns the credential lock.
type GatewayPendingCommit struct {
	AdmissionID, Owner, RuntimeID, RecordID uuid.UUID
	Sequence, AuthorizationRevision         int64
	PreviousHash, WireHash, Kind            string
	CreatedAt                               time.Time
	Audit                                   *AuditEvent
	ExpectedSecretRevision                  int64
}
