package server

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"time"

	"github.com/google/uuid"
	"github.com/sagehou/restfleet/internal/domain"
	"github.com/sagehou/restfleet/internal/security"
)

type GatewayGlobalAuditStore interface {
	RegisterGatewayAuditOrigin(context.Context, domain.GatewayAuditBinding, ed25519.PublicKey) (domain.GatewayAuditOrigin, error)
	GatewayAuditOrigin(context.Context, domain.GatewayAuditBinding) (domain.GatewayAuditOrigin, error)
	CommitGatewayGlobalAudit(context.Context, domain.GatewayAuditCommit) error
	CloseGatewayAuditOrigin(context.Context, domain.GatewayAuditBinding, int64, string) error
}

// RegisterGatewayGlobalAudit is central-only trusted provisioning. The source
// pin/runtime must be independently obtained, never selected by a replay frame.
// Registration permits audit ingestion only, not backup or credential access.
func (c *ControlPlane) RegisterGatewayGlobalAudit(ctx context.Context, b domain.GatewayAuditBinding, source ed25519.PublicKey) (domain.GatewayAuditOrigin, [32]byte, error) {
	var recipient [32]byte
	s, ok := c.store.(GatewayGlobalAuditStore)
	if !ok || b.Validate() != nil || len(source) != 32 || len(c.gatewayPendingKey) != 32 || len(c.gatewaySigningKey) != 64 {
		return domain.GatewayAuditOrigin{}, recipient, domain.ErrGatewayGlobalAudit
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	o, err := s.RegisterGatewayAuditOrigin(ctx, b, bytes.Clone(source))
	if err != nil || ctx.Err() != nil || o.Binding != b || !bytes.Equal(o.PublicKey, source) || o.ClosedAt != nil {
		return domain.GatewayAuditOrigin{}, recipient, domain.ErrGatewayGlobalAudit
	}
	recipient, err = security.GatewayPendingPublicKey(c.gatewayPendingKey)
	if err != nil { return domain.GatewayAuditOrigin{}, [32]byte{}, domain.ErrGatewayGlobalAudit }
	return o, recipient, nil
}

// ReplayGatewayRecord multiplexes the two strict wire domains on the existing
// protected, exact-peer Unix channel. Failed global verification never falls
// back to repository ingestion, and lookup selectors never choose audit IDs.
func (c *ControlPlane) ReplayGatewayRecord(ctx context.Context, runtime uuid.UUID, wire []byte) ([]byte, error) {
	b, err := security.GatewayPendingAuditIdentity(wire)
	if err != nil { return c.ReplayGatewayPending(ctx, runtime, wire) }
	return c.ReplayGatewayGlobalAudit(ctx, runtime, b, wire)
}

func (c *ControlPlane) ReplayGatewayGlobalAudit(ctx context.Context, runtime uuid.UUID, b domain.GatewayAuditBinding, wire []byte) (result []byte, failure error) {
	s, ok := c.store.(GatewayGlobalAuditStore)
	if !ok || b.Validate() != nil || b.RuntimeID != runtime || len(c.gatewayPendingKey) != 32 || len(c.gatewaySigningKey) != 64 || len(wire) > security.MaxGatewayPendingSize {
		return nil, domain.ErrGatewayGlobalAudit
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	defer func() {
		if failure != nil {
			audit, finish := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
			defer finish()
			_ = c.RecordGatewayReplayDenied(audit)
		}
	}()
	o, err := s.GatewayAuditOrigin(ctx, b)
	if err != nil || o.Binding != b { return nil, domain.ErrGatewayGlobalAudit }
	r, err := security.OpenGatewayPending(wire, o.PublicKey, c.gatewayPendingKey)
	if err != nil { return nil, domain.ErrGatewayGlobalAudit }
	defer clear(r.Config)
	h := r.Header
	if h.AuditOrigin != b || r.Kind != "global_audit" { return nil, domain.ErrGatewayGlobalAudit }
	audit, valid := domain.GatewayGlobalAudit(*r.Event)
	if !valid { return nil, domain.ErrGatewayGlobalAudit }
	audit.ID, audit.RequestID, audit.OccurredAt = h.RecordID, h.RecordID, time.Unix(h.CreatedAt, 0).UTC()
	if s.CommitGatewayGlobalAudit(ctx, domain.GatewayAuditCommit{Binding: b, RecordID: h.RecordID, Sequence: h.Sequence,
		PreviousHash: h.PreviousHash, WireHash: security.GatewayPendingHash(wire), CreatedAt: audit.OccurredAt, Audit: audit}) != nil || ctx.Err() != nil {
		return nil, domain.ErrGatewayGlobalAudit
	}
	ack, err := security.SignGatewayPendingReceipt(security.GatewayPendingReceipt{AuditOriginID: b.OriginID, RuntimeID: runtime,
		Sequence: h.Sequence, RecordID: h.RecordID, WireHash: security.GatewayPendingHash(wire)}, c.gatewaySigningKey)
	if err != nil || ctx.Err() != nil { return nil, domain.ErrGatewayGlobalAudit }
	return ack, nil
}

// SealGatewayGlobalAudit requires trusted ingress/producer cleanup and exact
// drain. This does not seal repository origins or release any backup fence.
func (c *ControlPlane) SealGatewayGlobalAudit(ctx context.Context, b domain.GatewayAuditBinding, sequence int64, hash string) error {
	s, ok := c.store.(GatewayGlobalAuditStore)
	if !ok || b.Validate() != nil || len(c.gatewayPendingKey) != 32 { return domain.ErrGatewayGlobalAudit }
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if s.CloseGatewayAuditOrigin(ctx, b, sequence, hash) != nil || ctx.Err() != nil { return domain.ErrGatewayGlobalAudit }
	return nil
}
