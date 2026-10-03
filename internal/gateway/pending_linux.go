package gateway

import (
	"bytes"
	"context"
	"sync"
	"time"

	"github.com/sagehou/restfleet/internal/gatewaypending"
	"github.com/sagehou/restfleet/internal/rclone"
	"github.com/sagehou/restfleet/internal/security"
)

// NewPendingAuditRecorder reuses the SAME allowlist as synchronous central
// audits. Acceptance means fsynced encrypted pending data, not DB commit. The
// caller owns the queue/runtime binding; this does not enable offline admission.
func NewPendingAuditRecorder(queue *gatewaypending.Queue, authorization *Authorization) (func(context.Context, Event) error, error) {
	if queue == nil || authorization == nil {
		return nil, ErrGatewayAudit
	}
	return func(ctx context.Context, event Event) error {
		_, valid := gatewayAudit(event)
		if ctx.Err() != nil {
			return ErrGatewayAudit
		}
		if !valid {
			event = Event{Action: "event_rejected", Reason: "invalid_event"}
		}
		authorization.mu.Lock()
		decision := authorization.lastGrant
		authorization.mu.Unlock()
		if decision.Revision == 0 {
			return ErrGatewayAudit
		}
		r := security.GatewayPendingRecord{Header: security.GatewayPendingHeader{Binding: authorization.binding, AuthorizationRevision: int64(decision.Revision), CreatedAt: time.Now().UTC().Unix()}, Kind: "audit", Event: &event}
		if queue.Append(r) != nil || ctx.Err() != nil || !valid {
			return ErrGatewayAudit
		}
		return nil
	}, nil
}

// NewPendingRefreshRecorder fits Runtime.WithGatewayConfig's existing watcher
// callback. It validates token-only changes locally, increments only after
// durable queue acceptance, and closes after its owner joined all callbacks.
// initialRevision/raw MUST be the registered central material revision.
func NewPendingRefreshRecorder(queue *gatewaypending.Queue, authorization *Authorization, raw []byte, remote string, initialRevision int64) (func(context.Context, []byte) error, func(), error) {
	if queue == nil || authorization == nil || initialRevision < 1 {
		return nil, nil, rclone.ErrRefreshPersist
	}
	previous, err := rclone.ParseConfig(string(raw), remote)
	if err != nil {
		return nil, nil, rclone.ErrRefreshPersist
	}
	var mu sync.Mutex
	closed := false
	revision := initialRevision
	closeRecorder := func() { mu.Lock(); defer mu.Unlock(); closed = true; previous = nil }
	persist := func(ctx context.Context, nextRaw []byte) error {
		mu.Lock()
		defer mu.Unlock()
		if closed || ctx.Err() != nil || authorization.Status() != AuthorizationValid {
			return rclone.ErrRefreshPersist
		}
		next, err := rclone.ParseConfig(string(nextRaw), remote)
		if err != nil || !previous.SameExceptToken(next) {
			return rclone.ErrConfigChanged
		}
		before, after := previous.Bytes(), next.Bytes()
		defer clear(before)
		defer clear(after)
		if bytes.Equal(before, after) {
			return nil
		}
		authorization.mu.Lock()
		decision := authorization.current
		now := authorization.sampleTime()
		valid := authorization.observeClock(now) && !decision.Revoked && decision.Revision > 0 && now.Unix() < decision.ExpiresAt && now.Before(authorization.deadline)
		var queued error
		if valid {
			queued = queue.Append(security.GatewayPendingRecord{Header: security.GatewayPendingHeader{Binding: authorization.binding, AuthorizationRevision: int64(decision.Revision), CreatedAt: time.Now().UTC().Unix()}, Kind: "refresh", ExpectedSecretRevision: revision, Config: after})
		}
		authorization.mu.Unlock()
		if !valid || queued != nil {
			return rclone.ErrRefreshPersist
		}
		previous = next
		revision++
		if ctx.Err() != nil || authorization.Status() != AuthorizationValid {
			return rclone.ErrRefreshPersist
		}
		return nil
	}
	return persist, closeRecorder, nil
}
