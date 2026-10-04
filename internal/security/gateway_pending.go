package security

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"

	"github.com/google/uuid"
	"github.com/sagehou/restfleet/internal/domain"
	"golang.org/x/crypto/curve25519"
	"golang.org/x/crypto/nacl/box"
)

const MaxGatewayPendingSize = 512 << 10
const pendingContext = "restfleet:gateway-pending:v1\x00"
const receiptContext = "restfleet:gateway-pending-receipt:v1\x00"
const globalPendingContext = "restfleet:gateway-global-audit:v1\x00"
const globalReceiptContext = "restfleet:gateway-global-audit-receipt:v1\x00"

type GatewayAuditBinding = domain.GatewayAuditBinding

var ErrGatewayPending = errors.New("gateway pending record unavailable or inconsistent")

type GatewayPendingHeader struct {
	Binding               GatewayAuthorizationBinding `json:"binding"`
	RecordID              uuid.UUID                   `json:"record_id"`
	Sequence              int64                       `json:"sequence"`
	PreviousHash          string                      `json:"previous_hash"`
	AuthorizationRevision int64                       `json:"authorization_revision"`
	CreatedAt             int64                       `json:"created_at"`
	AuditOrigin           GatewayAuditBinding         `json:"audit_origin,omitzero"`
}

func (h GatewayPendingHeader) Validate() error {
	global := h.AuditOrigin != (GatewayAuditBinding{})
	if (!global && (h.Binding.Validate() != nil || h.AuthorizationRevision < 1)) ||
		(global && (h.AuditOrigin.Validate() != nil || h.Binding != (GatewayAuthorizationBinding{}) || h.AuthorizationRevision != 0)) ||
		h.RecordID.Version() != 7 || h.RecordID.Variant() != uuid.RFC4122 ||
		h.Sequence < 1 || h.Sequence == math.MaxInt64 || h.CreatedAt <= 0 || h.CreatedAt > 253402300799 || !validPendingHash(h.PreviousHash) {
		return ErrGatewayPending
	}
	if h.Sequence == 1 && h.PreviousHash != hex.EncodeToString(make([]byte, 32)) {
		return ErrGatewayPending
	}
	return nil
}

// GatewayPendingRecord is short-lived plaintext, never logged or stored as JSON
// outside the encrypted queue. Config is solely for a token-only refresh.
type GatewayPendingRecord struct {
	Header                 GatewayPendingHeader `json:"header"`
	Kind                   string               `json:"kind"`
	Event                  *domain.GatewayEvent `json:"event"`
	ExpectedSecretRevision int64                `json:"expected_secret_revision"`
	Config                 []byte               `json:"config"`
}

func (r GatewayPendingRecord) Validate() error {
	if r.Header.Validate() != nil {
		return ErrGatewayPending
	}
	switch r.Kind {
	case "global_audit":
		if r.Header.AuditOrigin.Validate() != nil || r.Event == nil || r.ExpectedSecretRevision != 0 || r.Config != nil {
			return ErrGatewayPending
		}
		if _, ok := domain.GatewayGlobalAudit(*r.Event); !ok {
			return ErrGatewayPending
		}
	case "audit":
		if r.Header.AuditOrigin != (GatewayAuditBinding{}) || r.Event == nil || r.ExpectedSecretRevision != 0 || r.Config != nil {
			return ErrGatewayPending
		}
		if _, ok := domain.GatewayAudit(*r.Event); !ok {
			return ErrGatewayPending
		}
		b := r.Event.Binding
		if b != (domain.GatewayBinding{}) && (b.HostID != r.Header.Binding.HostID || b.RepositoryID != r.Header.Binding.RepositoryID || b.GatewayID != r.Header.Binding.GatewayID) {
			return ErrGatewayPending
		}
	case "refresh":
		if r.Header.AuditOrigin != (GatewayAuditBinding{}) || r.Event != nil || r.ExpectedSecretRevision < 1 || r.ExpectedSecretRevision == math.MaxInt64 || len(r.Config) == 0 || len(r.Config) > 256<<10 {
			return ErrGatewayPending
		}
	default:
		return ErrGatewayPending
	}
	return nil
}

type gatewayPendingWire struct {
	Header     GatewayPendingHeader `json:"header"`
	Ciphertext []byte               `json:"ciphertext"`
}

// GatewayPendingPublicKey derives the recipient key, which is safe to give to
// Gateway. The corresponding private key MUST stay in the central process.
func GatewayPendingPublicKey(private []byte) ([32]byte, error) {
	var result [32]byte
	if len(private) != 32 {
		return result, ErrGatewayPending
	}
	key, err := curve25519.X25519(private, curve25519.Basepoint)
	if err != nil {
		return result, ErrGatewayPending
	}
	copy(result[:], key)
	return result, nil
}

func SealGatewayPending(r GatewayPendingRecord, recipient [32]byte, source ed25519.PrivateKey) ([]byte, error) {
	if r.Validate() != nil || len(source) != ed25519.PrivateKeySize {
		return nil, ErrGatewayPending
	}
	derived := ed25519.NewKeyFromSeed(source[:32])
	defer clear(derived)
	if !bytes.Equal(derived, source) {
		return nil, ErrGatewayPending
	}
	plain, err := json.Marshal(r)
	if err != nil {
		return nil, ErrGatewayPending
	}
	defer clear(plain)
	sealed, err := box.SealAnonymous(nil, plain, &recipient, rand.Reader)
	if err != nil {
		return nil, ErrGatewayPending
	}
	payload, err := json.Marshal(gatewayPendingWire{r.Header, sealed})
	if err != nil || len(payload)+64 > MaxGatewayPendingSize {
		return nil, ErrGatewayPending
	}
	return append(payload, ed25519.Sign(source, append([]byte(pendingSignatureContext(r.Header)), payload...))...), nil
}

// Inspect verifies the registered source BEFORE decryption. Knowing the
// recipient public key alone cannot authenticate a pending record.
func InspectGatewayPending(wire []byte, source ed25519.PublicKey) (GatewayPendingHeader, error) {
	w, err := verifyPendingWire(wire, source)
	return w.Header, err
}

// GatewayPendingIdentity returns UNTRUSTED registry lookup selectors only.
// Callers MUST verify with that registered source key and compare the complete
// binding before using anything in the record. This grants no authority.
func GatewayPendingIdentity(wire []byte) (uuid.UUID, uuid.UUID, error) {
	if len(wire) <= 64 || len(wire) > MaxGatewayPendingSize {
		return uuid.Nil, uuid.Nil, ErrGatewayPending
	}
	var outer struct {
		Header GatewayPendingHeader `json:"header"`
	}
	if json.Unmarshal(wire[:len(wire)-64], &outer) != nil || outer.Header.Validate() != nil || outer.Header.AuditOrigin != (GatewayAuditBinding{}) {
		return uuid.Nil, uuid.Nil, ErrGatewayPending
	}
	return outer.Header.Binding.AdmissionID, outer.Header.Binding.RuntimeID, nil
}

// Global identity is an UNTRUSTED registry selector only, never authority.
// A regular record cannot choose this registry; verify the domain/source next.
func GatewayPendingAuditIdentity(wire []byte) (GatewayAuditBinding, error) {
	if len(wire) <= 64 || len(wire) > MaxGatewayPendingSize {
		return GatewayAuditBinding{}, ErrGatewayPending
	}
	var outer struct {
		Header GatewayPendingHeader `json:"header"`
	}
	if json.Unmarshal(wire[:len(wire)-64], &outer) != nil || outer.Header.Validate() != nil || outer.Header.AuditOrigin.Validate() != nil {
		return GatewayAuditBinding{}, ErrGatewayPending
	}
	return outer.Header.AuditOrigin, nil
}

func pendingSignatureContext(h GatewayPendingHeader) string {
	if h.AuditOrigin != (GatewayAuditBinding{}) {
		return globalPendingContext
	}
	return pendingContext
}

func verifyPendingWire(wire []byte, source ed25519.PublicKey) (gatewayPendingWire, error) {
	var w gatewayPendingWire
	if len(source) != 32 || len(wire) <= 64 || len(wire) > MaxGatewayPendingSize {
		return w, ErrGatewayPending
	}
	payload := wire[:len(wire)-64]
	var selector struct {
		Header GatewayPendingHeader `json:"header"`
	}
	if json.Unmarshal(payload, &selector) != nil || selector.Header.Validate() != nil ||
		!ed25519.Verify(source, append([]byte(pendingSignatureContext(selector.Header)), payload...), wire[len(payload):]) ||
		json.Unmarshal(payload, &w) != nil || len(w.Ciphertext) <= box.AnonymousOverhead {
		return gatewayPendingWire{}, ErrGatewayPending
	}
	canonical, err := json.Marshal(w)
	if err != nil || !bytes.Equal(canonical, payload) {
		return gatewayPendingWire{}, ErrGatewayPending
	}
	return w, nil
}

func OpenGatewayPending(wire []byte, source ed25519.PublicKey, private []byte) (GatewayPendingRecord, error) {
	var r GatewayPendingRecord
	w, err := verifyPendingWire(wire, source)
	public, keyErr := GatewayPendingPublicKey(private)
	if err != nil || keyErr != nil {
		return r, ErrGatewayPending
	}
	var key [32]byte
	copy(key[:], private)
	defer clear(key[:])
	plain, ok := box.OpenAnonymous(nil, w.Ciphertext, &public, &key)
	if !ok {
		return r, ErrGatewayPending
	}
	defer clear(plain)
	if json.Unmarshal(plain, &r) != nil || r.Header != w.Header || r.Validate() != nil {
		clear(r.Config)
		return GatewayPendingRecord{}, ErrGatewayPending
	}
	canonical, err := json.Marshal(r)
	defer clear(canonical)
	if err != nil || !bytes.Equal(canonical, plain) {
		clear(r.Config)
		return GatewayPendingRecord{}, ErrGatewayPending
	}
	return r, nil
}

func GatewayPendingHash(wire []byte) string {
	sum := sha256.Sum256(wire)
	return hex.EncodeToString(sum[:])
}
func validPendingHash(value string) bool {
	b, e := hex.DecodeString(value)
	return e == nil && len(b) == 32 && hex.EncodeToString(b) == value
}

// Receipt is a precise committed record acknowledgement, never a statement
// that a Gateway process stopped or that an admission can be released.
type GatewayPendingReceipt struct {
	AdmissionID   uuid.UUID `json:"admission_id"`
	RuntimeID     uuid.UUID `json:"runtime_id"`
	Sequence      int64     `json:"sequence"`
	RecordID      uuid.UUID `json:"record_id"`
	WireHash      string    `json:"wire_hash"`
	AuditOriginID uuid.UUID `json:"audit_origin_id,omitzero"`
}

func (r GatewayPendingReceipt) valid() bool {
	id := r.AdmissionID
	if r.AuditOriginID != uuid.Nil {
		if r.AdmissionID != uuid.Nil {
			return false
		}
		id = r.AuditOriginID
	}
	for _, id := range []uuid.UUID{id, r.RuntimeID, r.RecordID} {
		if id.Version() != 7 || id.Variant() != uuid.RFC4122 {
			return false
		}
	}
	return r.Sequence > 0 && r.Sequence < math.MaxInt64 && validPendingHash(r.WireHash)
}

func SignGatewayPendingReceipt(r GatewayPendingReceipt, key ed25519.PrivateKey) ([]byte, error) {
	if !r.valid() || len(key) != 64 {
		return nil, ErrGatewayPending
	}
	payload, err := json.Marshal(r)
	if err != nil {
		return nil, ErrGatewayPending
	}
	return append(payload, ed25519.Sign(key, append([]byte(receiptSignatureContext(r)), payload...))...), nil
}

func receiptSignatureContext(r GatewayPendingReceipt) string {
	if r.AuditOriginID != uuid.Nil {
		return globalReceiptContext
	}
	return receiptContext
}

func VerifyGatewayPendingReceipt(wire []byte, key ed25519.PublicKey) (GatewayPendingReceipt, error) {
	var r GatewayPendingReceipt
	if len(key) != 32 || len(wire) <= 64 || len(wire) > 1024 {
		return r, ErrGatewayPending
	}
	payload := wire[:len(wire)-64]
	if json.Unmarshal(payload, &r) != nil || !r.valid() || !ed25519.Verify(key, append([]byte(receiptSignatureContext(r)), payload...), wire[len(payload):]) {
		return GatewayPendingReceipt{}, ErrGatewayPending
	}
	canonical, err := json.Marshal(r)
	if err != nil || !bytes.Equal(payload, canonical) {
		return GatewayPendingReceipt{}, ErrGatewayPending
	}
	return r, nil
}
