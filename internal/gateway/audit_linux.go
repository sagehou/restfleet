package gateway

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/sagehou/restfleet/internal/domain"
)

// AuditStore commits the existing append-only audit chain synchronously.
// Implementations MUST honor cancellation. This is a CENTRAL trusted port,
// not permission to give a separate public Gateway the Server DB credentials.
type AuditStore interface {
	RecordAudit(context.Context, domain.AuditEvent) error
}

// NewAuditRecorder adapts fixed Gateway classifications to the existing audit
// chain. It adds no queue, logs, input-derived identity or offline authorization.
// A successful record acknowledges an observation/intent, not backup success
// or proof that a subsequent backend lock deletion completed.
func NewAuditRecorder(store AuditStore) (func(context.Context, Event) error, error) {
	if store == nil {
		return nil, ErrGatewayAudit
	}
	return func(ctx context.Context, event Event) error {
		ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		audit, valid := gatewayAudit(event)
		id, err := uuid.NewV7()
		if err != nil {
			return ErrGatewayAudit
		}
		audit.ID, audit.RequestID, audit.OccurredAt = id, id, time.Now().UTC()
		if store.RecordAudit(ctx, audit) != nil || ctx.Err() != nil || !valid {
			return ErrGatewayAudit
		}
		return nil
	}, nil
}

func gatewayAudit(event Event) (domain.AuditEvent, bool) {
	return domain.GatewayAudit(event)
}
