package security

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"math"

	"golang.org/x/crypto/nacl/box"
)

const MaxGatewayMaterialSize = 512 << 10

var ErrGatewayMaterial = errors.New("gateway material delivery unavailable or inconsistent")

const materialChallengeContext = "restfleet:gateway-material-challenge:v1\x00"
const materialContext = "restfleet:gateway-material:v1\x00"
const materialReceiptContext = "restfleet:gateway-material-receipt:v1\x00"

// GatewayMaterialChallenge binds a fresh connection to one pre-provisioned
// runtime/source. Recipient is a locally generated ephemeral encryption key,
// never a key supplied by an Agent or an unauthenticated request.
type GatewayMaterialChallenge struct {
	Binding   GatewayAuthorizationBinding `json:"binding"`
	Nonce     [32]byte                    `json:"nonce"`
	Recipient [32]byte                    `json:"recipient"`
}

func (c GatewayMaterialChallenge) valid() bool {
	return c.Binding.Validate() == nil && c.Nonce != ([32]byte{}) && c.Recipient != ([32]byte{})
}

func NewGatewayMaterialChallenge(binding GatewayAuthorizationBinding, recipient [32]byte) (GatewayMaterialChallenge, error) {
	c := GatewayMaterialChallenge{Binding: binding, Recipient: recipient}
	if _, err := rand.Read(c.Nonce[:]); err != nil || !c.valid() {
		return GatewayMaterialChallenge{}, ErrGatewayMaterial
	}
	return c, nil
}

func SignGatewayMaterialChallenge(c GatewayMaterialChallenge, source ed25519.PrivateKey) ([]byte, error) {
	if !c.valid() {
		return nil, ErrGatewayMaterial
	}
	return signMaterialJSON(c, source, materialChallengeContext, 2048)
}

func VerifyGatewayMaterialChallenge(wire []byte, source ed25519.PublicKey, binding GatewayAuthorizationBinding) (GatewayMaterialChallenge, error) {
	var c GatewayMaterialChallenge
	if verifyMaterialJSON(wire, source, materialChallengeContext, 2048, &c) != nil || !c.valid() || c.Binding != binding {
		return GatewayMaterialChallenge{}, ErrGatewayMaterial
	}
	return c, nil
}

// GatewayMaterial is borrowed plaintext solely for a Gateway initialization
// callback. It has no master/signing/recipient private key, DB credential,
// Restic password or Agent capability. Never log, marshal to disk or retain it.
type GatewayMaterial struct {
	Challenge          GatewayMaterialChallenge `json:"challenge"`
	Source             []byte                   `json:"source"`
	PendingRecipient   [32]byte                 `json:"pending_recipient"`
	Statement          []byte                   `json:"statement"`
	AdmissionCreatedAt int64                    `json:"admission_created_at"`
	AdmissionExpiresAt int64                    `json:"admission_expires_at"`
	SecretRevision     int64                    `json:"secret_revision"`
	Remote             string                   `json:"remote"`
	ConfigHash         string                   `json:"config_hash"`
	Config             []byte                   `json:"config"`
}

func (m GatewayMaterial) Validate(central ed25519.PublicKey) error {
	s, err := VerifyGatewayStatement(m.Statement, central)
	if !m.Challenge.valid() || len(m.Source) != ed25519.PublicKeySize || m.PendingRecipient == ([32]byte{}) || err != nil ||
		s.Binding != m.Challenge.Binding || s.Revoked || m.AdmissionCreatedAt <= 0 || m.AdmissionCreatedAt > s.IssuedAt ||
		m.AdmissionExpiresAt < s.ExpiresAt || m.AdmissionExpiresAt > 253402300799 ||
		m.AdmissionExpiresAt-m.AdmissionCreatedAt > 24*60*60 || m.SecretRevision < 1 || m.SecretRevision == math.MaxInt64 ||
		len(m.Remote) < 1 || len(m.Remote) > 64 || len(m.Config) == 0 || len(m.Config) > 256<<10 ||
		m.ConfigHash != GatewayPendingHash(m.Config) {
		return ErrGatewayMaterial
	}
	return nil
}

type gatewayMaterialWire struct {
	Challenge  GatewayMaterialChallenge `json:"challenge"`
	Source     []byte                   `json:"source"`
	Ciphertext []byte                   `json:"ciphertext"`
}

// SealGatewayMaterial MUST be called centrally only after current admission,
// latest decision, exact source/revision and secret-access audit commit.
func SealGatewayMaterial(m GatewayMaterial, central ed25519.PrivateKey) ([]byte, error) {
	if len(central) != 64 || m.Validate(central.Public().(ed25519.PublicKey)) != nil {
		return nil, ErrGatewayMaterial
	}
	plain, err := json.Marshal(m)
	if err != nil {
		return nil, ErrGatewayMaterial
	}
	defer clear(plain)
	sealed, err := box.SealAnonymous(nil, plain, &m.Challenge.Recipient, rand.Reader)
	if err != nil {
		return nil, ErrGatewayMaterial
	}
	return signMaterialJSON(gatewayMaterialWire{m.Challenge, m.Source, sealed}, central, materialContext, MaxGatewayMaterialSize)
}

func OpenGatewayMaterial(wire []byte, challenge GatewayMaterialChallenge, source, central ed25519.PublicKey, recipientPrivate []byte) (GatewayMaterial, error) {
	var w gatewayMaterialWire
	var m GatewayMaterial
	if !challenge.valid() || len(source) != 32 || verifyMaterialJSON(wire, central, materialContext, MaxGatewayMaterialSize, &w) != nil ||
		w.Challenge != challenge || !bytes.Equal(w.Source, source) || len(w.Ciphertext) <= box.AnonymousOverhead {
		return m, ErrGatewayMaterial
	}
	public, err := GatewayPendingPublicKey(recipientPrivate)
	if err != nil || public != challenge.Recipient {
		return m, ErrGatewayMaterial
	}
	var key [32]byte
	copy(key[:], recipientPrivate)
	defer clear(key[:])
	plain, ok := box.OpenAnonymous(nil, w.Ciphertext, &public, &key)
	if !ok {
		return m, ErrGatewayMaterial
	}
	defer clear(plain)
	if json.Unmarshal(plain, &m) != nil || m.Validate(central) != nil || m.Challenge != challenge || !bytes.Equal(m.Source, source) {
		clear(m.Config)
		return GatewayMaterial{}, ErrGatewayMaterial
	}
	canonical, err := json.Marshal(m)
	defer clear(canonical)
	if err != nil || !bytes.Equal(canonical, plain) {
		clear(m.Config)
		return GatewayMaterial{}, ErrGatewayMaterial
	}
	return m, nil
}

type GatewayMaterialReceipt struct {
	Challenge GatewayMaterialChallenge `json:"challenge"`
	WireHash  string                   `json:"wire_hash"`
}

// This receipt confirms initialization only, never Agent ACK, process cleanup,
// successful backup, public readiness or release of a central fence.
func SignGatewayMaterialReceipt(r GatewayMaterialReceipt, source ed25519.PrivateKey) ([]byte, error) {
	if !r.Challenge.valid() || !validPendingHash(r.WireHash) {
		return nil, ErrGatewayMaterial
	}
	return signMaterialJSON(r, source, materialReceiptContext, 2048)
}

func VerifyGatewayMaterialReceipt(wire []byte, challenge GatewayMaterialChallenge, hash string, source ed25519.PublicKey) error {
	var r GatewayMaterialReceipt
	if verifyMaterialJSON(wire, source, materialReceiptContext, 2048, &r) != nil || !challenge.valid() || r.Challenge != challenge ||
		!validPendingHash(r.WireHash) || r.WireHash != hash {
		return ErrGatewayMaterial
	}
	return nil
}

func signMaterialJSON(v any, key ed25519.PrivateKey, domain string, max int) ([]byte, error) {
	if len(key) != 64 {
		return nil, ErrGatewayMaterial
	}
	derived := ed25519.NewKeyFromSeed(key[:32])
	defer clear(derived)
	if !bytes.Equal(derived, key) {
		return nil, ErrGatewayMaterial
	}
	payload, err := json.Marshal(v)
	if err != nil || len(payload)+64 > max {
		return nil, ErrGatewayMaterial
	}
	return append(payload, ed25519.Sign(key, append([]byte(domain), payload...))...), nil
}

func verifyMaterialJSON(wire []byte, key ed25519.PublicKey, domain string, max int, v any) error {
	if len(key) != 32 || len(wire) <= 64 || len(wire) > max {
		return ErrGatewayMaterial
	}
	payload := wire[:len(wire)-64]
	if !ed25519.Verify(key, append([]byte(domain), payload...), wire[len(payload):]) || json.Unmarshal(payload, v) != nil {
		return ErrGatewayMaterial
	}
	canonical, err := json.Marshal(v)
	if err != nil || !bytes.Equal(payload, canonical) {
		return ErrGatewayMaterial
	}
	return nil
}
