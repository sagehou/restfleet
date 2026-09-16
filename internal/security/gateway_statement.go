package security

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"

	"github.com/google/uuid"
)

const gatewayStatementContext = "restfleet:gateway-authorization:v1\x00"
const MaxGatewayStatementSize = 2048
const MaxGatewayAuthorizationSeconds int64 = 12 * 60 * 60

var ErrGatewayStatement = errors.New("invalid gateway authorization statement")

// GatewayAuthorizationBinding contains references, never credential values.
// RuntimeID MUST be fresh for each trusted Gateway process incarnation.
type GatewayAuthorizationBinding struct {
	AdmissionID         uuid.UUID `json:"admission_id"`
	Owner               uuid.UUID `json:"owner"`
	RuntimeID           uuid.UUID `json:"runtime_id"`
	AgentID             uuid.UUID `json:"agent_id"`
	HostID              uuid.UUID `json:"host_id"`
	RepositoryID        uuid.UUID `json:"repository_id"`
	GatewayID           uuid.UUID `json:"gateway_id"`
	StorageCredentialID uuid.UUID `json:"storage_credential_id"`
	DeliveryID          uuid.UUID `json:"delivery_id"`
	GatewaySecretRef    uuid.UUID `json:"gateway_secret_ref"`
	ResticSecretRef     uuid.UUID `json:"restic_secret_ref"`
	ConfigurationHash   string    `json:"configuration_hash"`
}

func (b GatewayAuthorizationBinding) Validate() error {
	for _, id := range []uuid.UUID{b.AdmissionID, b.Owner, b.RuntimeID, b.AgentID, b.HostID, b.RepositoryID,
		b.GatewayID, b.StorageCredentialID, b.DeliveryID, b.GatewaySecretRef, b.ResticSecretRef} {
		if id.Version() != 7 || id.Variant() != uuid.RFC4122 {
			return ErrGatewayStatement
		}
	}
	hash, err := hex.DecodeString(b.ConfigurationHash)
	if err != nil || len(hash) != 32 || hex.EncodeToString(hash) != b.ConfigurationHash {
		return ErrGatewayStatement
	}
	return nil
}

// GatewayStatement is an internal, versioned central decision, not a public
// bearer capability. Revoked is an explicit decision, not transport failure.
// Times are UTC Unix seconds; revocation has ExpiresAt == 0 and is terminal.
type GatewayStatement struct {
	Binding   GatewayAuthorizationBinding `json:"binding"`
	Revision  uint64                      `json:"revision"`
	IssuedAt  int64                       `json:"issued_at"`
	ExpiresAt int64                       `json:"expires_at"`
	Revoked   bool                        `json:"revoked"`
}

func (s GatewayStatement) validate() error {
	// Bound timestamps before subtraction/conversion; year 9999 is the ceiling.
	if s.Binding.Validate() != nil || s.Revision == 0 || s.Revision > math.MaxInt64 ||
		s.IssuedAt <= 0 || s.IssuedAt > 253402300799 {
		return ErrGatewayStatement
	}
	if s.Revoked {
		if s.ExpiresAt != 0 {
			return ErrGatewayStatement
		}
	} else if s.ExpiresAt <= s.IssuedAt || s.ExpiresAt > 253402300799 ||
		s.ExpiresAt-s.IssuedAt > MaxGatewayAuthorizationSeconds {
		return ErrGatewayStatement
	}
	return nil
}

// SignGatewayStatement MUST only receive a DB-committed central decision after
// current identity/ACK/fence checks. The dedicated signing key stays central;
// do not reuse Agent CA keys. This helper itself does not establish authority.
func SignGatewayStatement(s GatewayStatement, key ed25519.PrivateKey) ([]byte, error) {
	if len(key) != ed25519.PrivateKeySize || s.validate() != nil {
		return nil, ErrGatewayStatement
	}
	payload, err := json.Marshal(s)
	if err != nil || len(payload)+ed25519.SignatureSize > MaxGatewayStatementSize {
		return nil, ErrGatewayStatement
	}
	signature := ed25519.Sign(key, append([]byte(gatewayStatementContext), payload...))
	return append(payload, signature...), nil
}

// VerifyGatewayStatement verifies the fixed v1 encoding and dedicated trusted
// public key, NOT liveness, local binding, replay state or cleanup. Callers MUST
// also enforce those boundaries. The wire contains no key or algorithm selector.
func VerifyGatewayStatement(wire []byte, key ed25519.PublicKey) (GatewayStatement, error) {
	if len(key) != ed25519.PublicKeySize || len(wire) <= ed25519.SignatureSize || len(wire) > MaxGatewayStatementSize {
		return GatewayStatement{}, ErrGatewayStatement
	}
	payload := wire[:len(wire)-ed25519.SignatureSize]
	if !ed25519.Verify(key, append([]byte(gatewayStatementContext), payload...), wire[len(payload):]) {
		return GatewayStatement{}, ErrGatewayStatement
	}
	var s GatewayStatement
	if json.Unmarshal(payload, &s) != nil || s.validate() != nil {
		return GatewayStatement{}, ErrGatewayStatement
	}
	canonical, err := json.Marshal(s)
	// Exact re-encoding rejects unknown/duplicate fields, alternate UUID forms,
	// trailing input and ambiguous representations, even when correctly signed.
	if err != nil || !bytes.Equal(payload, canonical) {
		return GatewayStatement{}, ErrGatewayStatement
	}
	return s, nil
}
