package gateway

import (
	"context"
	"crypto/ed25519"
	"sync"
	"time"

	"github.com/sagehou/restfleet/internal/domain"
	"github.com/sagehou/restfleet/internal/gatewaypending"
	"github.com/sagehou/restfleet/internal/security"
)

// GlobalAuditRecorder is the single process owner of a separately registered
// audit-only source. It never assigns a Host/Repository from request data.
// Failure latches: draining/reconnecting cannot revive a failed producer.
type GlobalAuditRecorder struct {
	mu sync.Mutex
	queue *gatewaypending.Queue
	binding security.GatewayAuditBinding
	source, central ed25519.PublicKey
	failed, closed bool
	lastWall time.Time
	clock func() time.Time
}

func NewGlobalAuditRecorder(q *gatewaypending.Queue, b security.GatewayAuditBinding, source, central ed25519.PublicKey) (*GlobalAuditRecorder, error) {
	if q == nil || q.ClaimGlobalAuditProducer(b, source, central) != nil { return nil, ErrGatewayAudit }
	return &GlobalAuditRecorder{queue: q, binding: b, source: append(ed25519.PublicKey(nil), source...), central: append(ed25519.PublicKey(nil), central...)}, nil
}

func (r *GlobalAuditRecorder) ready() bool {
	now := time.Now()
	if r.clock != nil { now = r.clock() }
	wall := now.Round(0)
	if now.IsZero() || wall.Before(r.lastWall) || r.closed || r.failed ||
		r.queue.CheckGlobalAuditProducer(r.binding, r.source, r.central) != nil {
		r.failed = true
		return false
	}
	r.lastWall = wall
	return true
}

func (r *GlobalAuditRecorder) Ready() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ready()
}

func (r *GlobalAuditRecorder) Record(ctx context.Context, event Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.ready() || ctx.Err() != nil { r.failed = true; return ErrGatewayAudit }
	_, valid := domain.GatewayGlobalAudit(event)
	if !valid { event = Event{Action: "event_rejected", Reason: "invalid_event"} }
	if r.queue.Append(security.GatewayPendingRecord{Header: security.GatewayPendingHeader{AuditOrigin: r.binding, CreatedAt: r.lastWall.UTC().Unix()}, Kind: "global_audit", Event: &event}) != nil || ctx.Err() != nil || !valid {
		r.failed = true
		return ErrGatewayAudit
	}
	return nil
}

// Close is called after all ingress/recording work joins. Queue remains owned
// by the caller for authenticated replay; this is not central cleanup proof.
func (r *GlobalAuditRecorder) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	r.queue.Freeze()
}
