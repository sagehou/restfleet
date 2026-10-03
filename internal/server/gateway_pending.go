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

// GatewayPendingStore is a CENTRAL port. Gateway has neither this port's DB
// credentials nor decryption/master/signing keys.
type GatewayPendingStore interface {
	RegisterGatewayPendingOrigin(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, ed25519.PublicKey, string) (domain.GatewayPendingOrigin, error)
	GatewayPendingOrigin(context.Context, uuid.UUID, uuid.UUID) (domain.GatewayPendingOrigin, error)
	CommitGatewayPending(context.Context, domain.GatewayPendingCommit, func(domain.StorageCredential, domain.SecretEnvelope) (domain.SecretEnvelope, error)) error
	CloseGatewayPendingOrigin(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, int64, string) error
}

func pendingBinding(o domain.GatewayPendingOrigin) security.GatewayAuthorizationBinding {
	a := o.Admission
	return security.GatewayAuthorizationBinding{AdmissionID: a.ID, Owner: a.Owner, RuntimeID: o.RuntimeID, AgentID: a.AgentID, HostID: a.HostID,
		RepositoryID: a.RepositoryID, GatewayID: a.GatewayID, StorageCredentialID: a.StorageCredentialID, DeliveryID: a.DeliveryID,
		GatewaySecretRef: a.GatewaySecretRef, ResticSecretRef: a.ResticSecretRef, ConfigurationHash: a.ConfigurationHash}
}

// RegisterGatewayPending is for a trusted coordinator BEFORE material delivery.
// The source key MUST come from its protected runtime provisioning, not replay.
func (c *ControlPlane) RegisterGatewayPending(ctx context.Context, binding security.GatewayAuthorizationBinding, source ed25519.PublicKey) (domain.GatewayPendingOrigin, [32]byte, error) {
	var public [32]byte
	s, ok := c.store.(GatewayPendingStore)
	if !ok || binding.Validate() != nil || binding.ConfigurationHash != c.credentialConfigurationHash() || len(c.gatewaySigningKey) != 64 || len(c.gatewayPendingKey) != 32 || len(source) != 32 {
		return domain.GatewayPendingOrigin{}, public, domain.ErrGatewayPending
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	o, err := s.RegisterGatewayPendingOrigin(ctx, binding.AdmissionID, binding.Owner, binding.RuntimeID, source, binding.ConfigurationHash)
	if err != nil || ctx.Err() != nil || pendingBinding(o) != binding || !bytes.Equal(o.PublicKey, source) || o.ClosedAt != nil {
		return domain.GatewayPendingOrigin{}, public, domain.ErrGatewayPending
	}
	public, err = security.GatewayPendingPublicKey(c.gatewayPendingKey)
	if err != nil {
		return domain.GatewayPendingOrigin{}, [32]byte{}, domain.ErrGatewayPending
	}
	return o, public, nil
}

// ReplayGatewayPending accepts a bounded SOURCE-authenticated encrypted record.
// It signs an exact ACK only AFTER the effect and replay history commit. It is
// not a public API, a new admission, or cleanup/owner takeover authorization.
func (c *ControlPlane) ReplayGatewayPending(ctx context.Context, runtime uuid.UUID, wire []byte) (result []byte, failure error) {
	s, ok := c.store.(GatewayPendingStore)
	if !ok || len(c.gatewayPendingKey) != 32 || len(c.gatewaySigningKey) != 64 || len(c.masterKey) != 32 || runtime.Version() != 7 || runtime.Variant() != uuid.RFC4122 || len(wire) > security.MaxGatewayPendingSize {
		return nil, domain.ErrGatewayPending
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	defer func() {
		if failure != nil {
			_ = c.RecordGatewayReplayDenied(ctx)
		}
	}()
	id, claimedRuntime, err := security.GatewayPendingIdentity(wire)
	if err != nil || claimedRuntime != runtime {
		return nil, domain.ErrGatewayPending
	}
	o, err := s.GatewayPendingOrigin(ctx, id, runtime)
	if err != nil || o.RuntimeID != runtime {
		return nil, domain.ErrGatewayPending
	}
	r, err := security.OpenGatewayPending(wire, o.PublicKey, c.gatewayPendingKey)
	if err != nil {
		return nil, domain.ErrGatewayPending
	}
	defer clear(r.Config)
	h := r.Header
	if h.Binding != pendingBinding(o) {
		return nil, domain.ErrGatewayPending
	}
	commit := domain.GatewayPendingCommit{AdmissionID: h.Binding.AdmissionID, Owner: h.Binding.Owner, RuntimeID: runtime, RecordID: h.RecordID,
		Sequence: h.Sequence, AuthorizationRevision: h.AuthorizationRevision, PreviousHash: h.PreviousHash, WireHash: security.GatewayPendingHash(wire),
		CreatedAt: time.Unix(h.CreatedAt, 0).UTC(), Kind: r.Kind, ExpectedSecretRevision: r.ExpectedSecretRevision}
	var validate func(domain.StorageCredential, domain.SecretEnvelope) (domain.SecretEnvelope, error)
	if r.Kind == "audit" {
		audit, valid := domain.GatewayAudit(*r.Event)
		if !valid {
			return nil, domain.ErrGatewayPending
		}
		audit.ID = h.RecordID
		audit.RequestID = h.RecordID
		audit.OccurredAt = commit.CreatedAt
		commit.Audit = &audit
	} else {
		validate = func(credential domain.StorageCredential, envelope domain.SecretEnvelope) (domain.SecretEnvelope, error) {
			raw, err := openStorageSecret(c.masterKey, credential, envelope)
			if err != nil {
				return domain.SecretEnvelope{}, domain.ErrGatewayPending
			}
			defer clear(raw)
			previous, err := rclone.ParseConfig(string(raw), credential.RemoteName)
			if err != nil || storageProvider(previous) != credential.Provider {
				return domain.SecretEnvelope{}, domain.ErrGatewayPending
			}
			next, err := rclone.ParseConfig(string(r.Config), credential.RemoteName)
			if err != nil || !previous.SameExceptToken(next) {
				return domain.SecretEnvelope{}, domain.ErrGatewayPending
			}
			before, after := previous.Bytes(), next.Bytes()
			defer clear(before)
			defer clear(after)
			if bytes.Equal(before, after) {
				return domain.SecretEnvelope{}, domain.ErrGatewayPending
			}
			credential.SecretRevision++
			credential.UpdatedAt = c.clock().UTC()
			return c.sealStorageConfig(credential, next)
		}
	}
	if s.CommitGatewayPending(ctx, commit, validate) != nil || ctx.Err() != nil {
		return nil, domain.ErrGatewayPending
	}
	receipt := security.GatewayPendingReceipt{AdmissionID: h.Binding.AdmissionID, RuntimeID: runtime, Sequence: h.Sequence, RecordID: h.RecordID, WireHash: commit.WireHash}
	ack, err := security.SignGatewayPendingReceipt(receipt, c.gatewaySigningKey)
	if err != nil || ctx.Err() != nil {
		return nil, domain.ErrGatewayPending
	}
	return ack, nil
}

// SealGatewayPending MUST be called only after trusted cleanup and exact drain.
// The store verifies the tail, not process exit. No public route exposes it.
func (c *ControlPlane) SealGatewayPending(ctx context.Context, id, owner, runtime uuid.UUID, sequence int64, hash string) error {
	s, ok := c.store.(GatewayPendingStore)
	if !ok || len(c.gatewayPendingKey) != 32 {
		return domain.ErrGatewayPending
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if s.CloseGatewayPendingOrigin(ctx, id, owner, runtime, sequence, hash) != nil || ctx.Err() != nil {
		return domain.ErrGatewayPending
	}
	return nil
}

// RecordGatewayReplayDenied accepts no input-derived identity or diagnostics.
func (c *ControlPlane) RecordGatewayReplayDenied(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return c.RecordDenied(ctx, "GATEWAY_PENDING_REPLAY_DENIED", "GATEWAY", "REJECTED", RequestMeta{})
}
