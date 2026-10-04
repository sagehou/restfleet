package server

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"time"

	"github.com/sagehou/restfleet/internal/domain"
	"github.com/sagehou/restfleet/internal/gatewaypending"
	"github.com/sagehou/restfleet/internal/security"
)

// DeliverGatewayAuthorization is central-only, after initial material delivery.
// The trusted coordinator provides binding/source/path independently and MUST
// serialize this with all decisions, initialization and cleanup for admission.
// It never registers, decrypts/reissues material, seals or releases anything.
func (c *ControlPlane) DeliverGatewayAuthorization(ctx context.Context, path string, gatewayUID uint32,
	binding security.GatewayAuthorizationBinding, source ed25519.PublicKey, r domain.GatewayDecisionRequest, sharedGroup ...uint32,
) error {
	store, ok := c.store.(GatewayPendingStore)
	if !ok || binding.Validate() != nil || len(source) != 32 || len(c.gatewaySigningKey) != 64 ||
		r.Validate() != nil || r.ExpectedRevision < 1 || r.AdmissionID != binding.AdmissionID || r.Owner != binding.Owner || r.RuntimeID != binding.RuntimeID {
		return security.ErrGatewayAuthority
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := c.Ready(ctx); err != nil || ctx.Err() != nil {
		return security.ErrGatewayAuthority
	}
	source = append(ed25519.PublicKey(nil), source...)
	err := gatewaypending.SendAuthorization(ctx, path, gatewayUID, binding, source, func(ctx context.Context) ([]byte, error) {
		o, err := store.GatewayPendingOrigin(ctx, binding.AdmissionID, binding.RuntimeID)
		if err != nil || o.ClosedAt != nil || o.Admission.ReleasedAt != nil || pendingBinding(o) != binding || !bytes.Equal(o.PublicKey, source) {
			return nil, security.ErrGatewayAuthority
		}
		wire, err := c.DecideGatewayAuthorization(ctx, r)
		if err != nil {
			return nil, security.ErrGatewayAuthority
		}
		statement, err := security.VerifyGatewayStatement(wire, c.gatewaySigningKey.Public().(ed25519.PublicKey))
		if err != nil || statement.Binding != binding || statement.Revision != uint64(r.ExpectedRevision+1) || statement.Revoked != r.Revoke {
			return nil, security.ErrGatewayAuthority
		}
		return wire, nil
	}, c.RecordGatewayAuthorityDenied, sharedGroup...)
	if err != nil {
		return security.ErrGatewayAuthority
	}
	return nil
}

func (c *ControlPlane) RecordGatewayAuthorityDenied(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return c.RecordDenied(ctx, "GATEWAY_AUTHORIZATION_DELIVERY_DENIED", "GATEWAY", "REJECTED", RequestMeta{})
}
