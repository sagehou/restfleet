package security

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
)

var ErrGatewayAuthority = errors.New("gateway authorization delivery unavailable or inconsistent")

const authorityChallengeContext = "restfleet:gateway-authority-challenge:v1\x00"
const authorityReceiptContext = "restfleet:gateway-authority-receipt:v1\x00"

// GatewayAuthorityChallenge authenticates a fresh metadata-only exchange. Its
// source pin/binding must be independently provisioned; it carries no keys.
type GatewayAuthorityChallenge struct {
	Binding GatewayAuthorizationBinding `json:"binding"`
	Nonce   [32]byte                    `json:"nonce"`
}

func (c GatewayAuthorityChallenge) valid() bool {
	return c.Binding.Validate() == nil && c.Nonce != ([32]byte{})
}

func NewGatewayAuthorityChallenge(binding GatewayAuthorizationBinding) (GatewayAuthorityChallenge, error) {
	c := GatewayAuthorityChallenge{Binding: binding}
	if _, err := rand.Read(c.Nonce[:]); err != nil || !c.valid() {
		return GatewayAuthorityChallenge{}, ErrGatewayAuthority
	}
	return c, nil
}

func SignGatewayAuthorityChallenge(c GatewayAuthorityChallenge, source ed25519.PrivateKey) ([]byte, error) {
	if !c.valid() {
		return nil, ErrGatewayAuthority
	}
	wire, err := signMaterialJSON(c, source, authorityChallengeContext, 2048)
	if err != nil {
		return nil, ErrGatewayAuthority
	}
	return wire, nil
}

func VerifyGatewayAuthorityChallenge(wire []byte, source ed25519.PublicKey, binding GatewayAuthorizationBinding) (GatewayAuthorityChallenge, error) {
	var c GatewayAuthorityChallenge
	if verifyMaterialJSON(wire, source, authorityChallengeContext, 2048, &c) != nil || !c.valid() || c.Binding != binding {
		return GatewayAuthorityChallenge{}, ErrGatewayAuthority
	}
	return c, nil
}

// GatewayAuthorityReceipt confirms acceptance of the exact signed statement,
// not data-plane readiness, completed cancellation/cleanup or fence release.
type GatewayAuthorityReceipt struct {
	Challenge GatewayAuthorityChallenge `json:"challenge"`
	WireHash  string                    `json:"wire_hash"`
}

func SignGatewayAuthorityReceipt(r GatewayAuthorityReceipt, source ed25519.PrivateKey) ([]byte, error) {
	if !r.Challenge.valid() || !validPendingHash(r.WireHash) {
		return nil, ErrGatewayAuthority
	}
	wire, err := signMaterialJSON(r, source, authorityReceiptContext, 2048)
	if err != nil {
		return nil, ErrGatewayAuthority
	}
	return wire, nil
}

func VerifyGatewayAuthorityReceipt(wire []byte, challenge GatewayAuthorityChallenge, hash string, source ed25519.PublicKey) error {
	var r GatewayAuthorityReceipt
	if verifyMaterialJSON(wire, source, authorityReceiptContext, 2048, &r) != nil || !challenge.valid() ||
		r.Challenge != challenge || !validPendingHash(r.WireHash) || r.WireHash != hash {
		return ErrGatewayAuthority
	}
	return nil
}
