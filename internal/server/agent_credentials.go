package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"

	"github.com/google/uuid"
	"github.com/sagehou/restfleet/internal/domain"
)

// WithAgentCredential is mTLS-only. agentID MUST come from the verified peer,
// never a request payload. The callback must not retain or log the material.
func (c *ControlPlane) WithAgentCredential(ctx context.Context, agentID uuid.UUID, force bool, send func(domain.RepositoryCredential) error) error {
	if c.gatewayPublicURL == "" {
		return nil
	}
	if send == nil {
		return domain.ErrRepositoryCredential
	}
	d, err := c.store.PrepareAgentCredential(ctx, agentID, c.credentialConfigurationHash(), force)
	if errors.Is(err, domain.ErrNotFound) {
		return nil
	}
	if err != nil {
		return domain.ErrRepositoryCredential
	}
	r := d.Repository
	value := domain.RepositoryCredential{DeliveryID: d.ID, AgentID: agentID, HostID: r.HostID, RepositoryID: r.ID, GatewayID: r.GatewayID,
		Revision: d.Revision, GatewayRevision: r.GatewaySecretRevision, ResticRevision: r.ResticSecretRevision, ValidFrom: d.CreatedAt,
		Endpoint: c.gatewayPublicURL + "/restic/" + r.GatewayID.String() + "/" + r.ID.String() + "/", CABundlePEM: c.enrollment.ServerCABundlePEM}
	value.GatewayPassword, err = openRepositorySecret(c.masterKey, r, "GATEWAY", d.Gateway)
	if err != nil {
		return domain.ErrRepositoryCredential
	}
	defer func() { value.Clear() }()
	value.ResticPassword, err = openRepositorySecret(c.masterKey, r, "RESTIC_KEY", d.Restic)
	if err != nil || value.Validate() != nil {
		return domain.ErrRepositoryCredential
	}
	if err := send(value); err != nil {
		return domain.ErrRepositoryCredential
	}
	return nil
}

func (c *ControlPlane) AcceptAgentCredential(ctx context.Context, agentID, deliveryID uuid.UUID, revision int64) error {
	if c.gatewayPublicURL == "" || deliveryID.Version() != 7 || deliveryID.Variant() != uuid.RFC4122 || revision < 1 {
		return domain.ErrRepositoryCredential
	}
	return c.store.AcceptAgentCredential(ctx, agentID, deliveryID, revision, c.credentialConfigurationHash())
}

func (c *ControlPlane) credentialConfigurationHash() string {
	hash := sha256.Sum256(append([]byte(c.gatewayPublicURL+"\n"), c.enrollment.ServerCABundlePEM...))
	return hex.EncodeToString(hash[:])
}
