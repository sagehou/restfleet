package domain

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

const (
	HostPending  = "PENDING"
	HostActive   = "ACTIVE"
	HostDisabled = "DISABLED"
	HostRevoked  = "REVOKED"

	AgentActive  = "ACTIVE"
	AgentRevoked = "REVOKED"
)

var (
	ErrRevisionConflict       = errors.New("revision conflict")
	ErrAlreadyExists          = errors.New("already exists")
	ErrEnrollmentTokenInvalid = errors.New("enrollment token is invalid")
	ErrEnrollmentUnavailable  = errors.New("agent enrollment is unavailable")
	ErrAgentRevoked           = errors.New("agent is revoked")
)

// Host is the control-plane identity of one protected machine.
type Host struct {
	ID          uuid.UUID
	DisplayName string
	Description string
	Labels      map[string]string
	Timezone    string
	Status      string
	Revision    int64
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Agent is one installed RestFleet agent identity. Its private key is never stored centrally.
type Agent struct {
	ID                   uuid.UUID
	HostID               uuid.UUID
	InstallID            uuid.UUID
	PublicKeyFingerprint string
	CertificateSerial    string
	CertificateNotAfter  time.Time
	Status               string
	Version              string
	ProtocolVersion      string
	OS                   string
	Arch                 string
	Hostname             string
	BootID               string
	ResticVersion        string
	LastSeenAt           *time.Time
	LastConnectedAt      *time.Time
	DesiredRevision      int64
	AcceptedRevision     int64
	Health               string
	UptimeSeconds        int64
	StateFreeBytes       int64
	ClockOffsetMS        int64
	HeartbeatErrorCode   string
	ConfigErrorCode      string
	ConfigErrorField     string
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

type EnrollmentToken struct {
	ID            uuid.UUID
	HostID        uuid.UUID
	TokenHash     []byte
	Fingerprint   string
	ExpiresAt     time.Time
	CreatedBy     uuid.UUID
	CreatedAt     time.Time
	UsedAt        *time.Time
	UsedByAgentID *uuid.UUID
	RevokedAt     *time.Time
}

func (t EnrollmentToken) Status(now time.Time) string {
	switch {
	case t.RevokedAt != nil:
		return "REVOKED"
	case t.UsedAt != nil:
		return "USED"
	case !now.Before(t.ExpiresAt):
		return "EXPIRED"
	default:
		return "ACTIVE"
	}
}

type AgentCertificate struct {
	ID                   uuid.UUID
	AgentID              uuid.UUID
	SerialNumber         string
	PublicKeyFingerprint string
	NotBefore            time.Time
	NotAfter             time.Time
	IssuedAt             time.Time
	CertificatePEM       []byte
	RevokedAt            *time.Time
	RevocationReason     string
	SupersededBy         *uuid.UUID
	OverlapEndsAt        *time.Time
}

// SecretEnvelope contains an encrypted payload and an independently wrapped data key.
type SecretEnvelope struct {
	ID             uuid.UUID
	Kind           string
	Algorithm      string
	KeyID          string
	Ciphertext     []byte
	Nonce          []byte
	WrappedDataKey []byte
	WrapNonce      []byte
	AAD            []byte
	CreatedAt      time.Time
}

type AgentCARecord struct {
	CertificatePEM []byte
	PrivateKey     SecretEnvelope
	CreatedAt      time.Time
}

type EnrollmentMaterial struct {
	Agent          Agent
	Certificate    AgentCertificate
	CertificatePEM []byte
	Audit          AuditEvent
	DesiredState   DesiredState
	OutboxID       uuid.UUID
}

type EnrollmentIssuer func(hostID uuid.UUID) (EnrollmentMaterial, error)
