package server

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"time"

	"github.com/google/uuid"
	"github.com/sagehou/restfleet/internal/domain"
	"github.com/sagehou/restfleet/internal/rclone"
	"github.com/sagehou/restfleet/internal/security"
)

type GatewayMaterialStore interface {
	GatewayDeliveryMaterial(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, ed25519.PublicKey, string, string) (domain.GatewayDeliveryMaterial, error)
}

// GatewayMaterialDelivery authenticates a fresh Gateway challenge against the
// trusted coordinator's pinned source/binding. Those pins MUST NOT come from
// Agent/replay requests. Current DB state and secret access must commit before
// signing/encryption. Only encrypted wire leaves this central service. There is
// no public API, automatic source registration or authorization issuance here.
func (c *ControlPlane) GatewayMaterialDelivery(ctx context.Context, binding security.GatewayAuthorizationBinding, source ed25519.PublicKey, challengeWire []byte) (wire []byte, failure error) {
	store, ok := c.store.(GatewayMaterialStore)
	if !ok || binding.Validate() != nil || len(source) != 32 || len(c.gatewaySigningKey) != 64 || len(c.gatewayPendingKey) != 32 || len(c.masterKey) != 32 || binding.ConfigurationHash != c.credentialConfigurationHash() {
		return nil, security.ErrGatewayMaterial
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	defer func() {
		if failure != nil {
			_ = c.RecordGatewayMaterialDenied(ctx)
		}
	}()
	challenge, err := security.VerifyGatewayMaterialChallenge(challengeWire, source, binding)
	if err != nil {
		return nil, security.ErrGatewayMaterial
	}
	result, err := store.GatewayDeliveryMaterial(ctx, binding.AdmissionID, binding.Owner, binding.RuntimeID, source, binding.ConfigurationHash, security.GatewayPendingHash(challengeWire))
	if err != nil || ctx.Err() != nil || pendingBinding(result.Origin) != binding || !bytes.Equal(result.Origin.PublicKey, source) || result.Origin.ClosedAt != nil {
		return nil, security.ErrGatewayMaterial
	}
	a, d, material := result.Origin.Admission, result.Decision, result.Material
	if material.Admission != a || d.Admission != a || d.RuntimeID != binding.RuntimeID || d.Revoked || d.Revision < 1 || a.ReleasedAt != nil ||
		!a.ExpiresAt.After(c.clock()) || !d.ExpiresAt.After(c.clock()) || d.IssuedAt.After(c.clock()) || d.ExpiresAt.After(a.ExpiresAt) ||
		d.ExpiresAt.Sub(d.IssuedAt) > 12*time.Hour || material.Credential.ID != binding.StorageCredentialID ||
		material.Credential.SecretRevision != result.Origin.InitialSecretRevision {
		return nil, security.ErrGatewayMaterial
	}
	raw, err := openStorageSecret(c.masterKey, material.Credential, material.Envelope)
	if err != nil {
		return nil, security.ErrGatewayMaterial
	}
	defer clear(raw)
	config, err := rclone.ParseConfig(string(raw), material.Credential.RemoteName)
	if err != nil || storageProvider(config) != material.Credential.Provider {
		return nil, security.ErrGatewayMaterial
	}
	canonical := config.Bytes()
	defer clear(canonical)
	statement, err := security.SignGatewayStatement(security.GatewayStatement{Binding: binding, Revision: uint64(d.Revision), IssuedAt: d.IssuedAt.Unix(), ExpiresAt: d.ExpiresAt.Unix()}, c.gatewaySigningKey)
	if err != nil {
		return nil, security.ErrGatewayMaterial
	}
	recipient, err := security.GatewayPendingPublicKey(c.gatewayPendingKey)
	if err != nil {
		return nil, security.ErrGatewayMaterial
	}
	wire, err = security.SealGatewayMaterial(security.GatewayMaterial{Challenge: challenge, Source: source, PendingRecipient: recipient,
		Statement: statement, AdmissionCreatedAt: a.CreatedAt.Truncate(time.Second).Unix(), AdmissionExpiresAt: a.ExpiresAt.Truncate(time.Second).Unix(),
		SecretRevision: material.Credential.SecretRevision, Remote: material.Credential.RemoteName, ConfigHash: security.GatewayPendingHash(canonical), Config: canonical}, c.gatewaySigningKey)
	if err != nil || ctx.Err() != nil || !d.ExpiresAt.After(c.clock()) || !a.ExpiresAt.After(c.clock()) {
		return nil, security.ErrGatewayMaterial
	}
	return wire, nil
}

func (c *ControlPlane) RecordGatewayMaterialDenied(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return c.RecordDenied(ctx, "GATEWAY_MATERIAL_DELIVERY_DENIED", "GATEWAY", "REJECTED", RequestMeta{})
}
