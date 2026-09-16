package server

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"time"

	"github.com/sagehou/restfleet/internal/domain"
	"github.com/sagehou/restfleet/internal/security"
)

type GatewayDecisionStore interface {
	DecideGatewayAuthorization(context.Context, domain.GatewayDecisionRequest, string) (domain.GatewayDecision, error)
}

// DecideGatewayAuthorization is for the trusted CENTRAL runtime coordinator,
// never an Agent/admin HTTP endpoint. The caller must serialize decision delivery
// and final cleanup/release for this runtime; a signature is not a new admission.
// The key is loaded centrally and MUST NOT be passed to a Gateway runner.
func (c *ControlPlane) DecideGatewayAuthorization(ctx context.Context, r domain.GatewayDecisionRequest) ([]byte, error) {
	store, ok := c.store.(GatewayDecisionStore)
	if !ok || len(c.gatewaySigningKey) == 0 || c.gatewayPublicURL == "" || r.Validate() != nil {
		return nil, domain.ErrGatewayDecision
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	d, err := store.DecideGatewayAuthorization(ctx, r, c.credentialConfigurationHash())
	if err != nil || ctx.Err() != nil {
		return nil, domain.ErrGatewayDecision
	}
	a := d.Admission
	if d.RequestID != r.ID || a.ID != r.AdmissionID || a.Owner != r.Owner || d.RuntimeID != r.RuntimeID ||
		d.Revision != r.ExpectedRevision+1 || d.Revoked != r.Revoke || d.RequestedLifetime != r.Lifetime || d.IssuedAt.After(c.clock()) {
		return nil, domain.ErrGatewayDecision
	}
	if !d.Revoked && (a.ReleasedAt != nil || a.ConfigurationHash != c.credentialConfigurationHash() || !d.ExpiresAt.After(c.clock()) ||
		d.ExpiresAt.After(a.ExpiresAt) || d.ExpiresAt.Sub(d.IssuedAt) > r.Lifetime) {
		return nil, domain.ErrGatewayDecision
	}
	s := security.GatewayStatement{Binding: security.GatewayAuthorizationBinding{
		AdmissionID: a.ID, Owner: a.Owner, RuntimeID: d.RuntimeID, AgentID: a.AgentID, HostID: a.HostID, RepositoryID: a.RepositoryID,
		GatewayID: a.GatewayID, StorageCredentialID: a.StorageCredentialID, DeliveryID: a.DeliveryID,
		GatewaySecretRef: a.GatewaySecretRef, ResticSecretRef: a.ResticSecretRef, ConfigurationHash: a.ConfigurationHash,
	}, Revision: uint64(d.Revision), IssuedAt: d.IssuedAt.Unix(), Revoked: d.Revoked}
	if !d.Revoked {
		s.ExpiresAt = d.ExpiresAt.Unix()
	}
	wire, err := security.SignGatewayStatement(s, c.gatewaySigningKey)
	if err != nil || ctx.Err() != nil {
		return nil, domain.ErrGatewayDecision
	}
	return wire, nil
}

func validGatewaySigningKey(key ed25519.PrivateKey) bool {
	if len(key) != ed25519.PrivateKeySize {
		return false
	}
	derived := ed25519.NewKeyFromSeed(key[:ed25519.SeedSize])
	defer clear(derived)
	return bytes.Equal(derived, key)
}
