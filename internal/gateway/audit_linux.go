package gateway

import (
	"context"
	"encoding/json"
	"strings"
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
	invalid := domain.AuditEvent{ActorType: domain.ActorSystem, Action: "GATEWAY_EVENT_REJECTED",
		ResourceType: "GATEWAY", Result: domain.AuditDenied, ReasonCode: "INVALID_EVENT"}
	scoped := event.Binding != (Binding{})
	if scoped {
		for _, id := range []uuid.UUID{event.Binding.HostID, event.Binding.RepositoryID, event.Binding.GatewayID, event.Binding.OperationID} {
			if id.Version() != 7 || id.Variant() != uuid.RFC4122 {
				return invalid, false
			}
		}
	}
	action, result := "", domain.AuditSuccess
	switch event.Action {
	case "session_start":
		if !scoped || event.Authenticated || event.Reason != "requested" {
			return invalid, false
		}
		action = "GATEWAY_SESSION_START"
	case "session_end":
		if !scoped || event.Authenticated || event.Reason != "finished" {
			return invalid, false
		}
		action = "GATEWAY_SESSION_END"
	case "lock_cleanup":
		if !scoped || !event.Authenticated || event.Reason != "owned_lock" {
			return invalid, false
		}
		action = "GATEWAY_LOCK_CLEANUP_INTENT"
	case "denied":
		action, result = "GATEWAY_REQUEST_DENIED", domain.AuditDenied
		switch event.Reason {
		case "route_unavailable", "rate_limited":
			if scoped || event.Authenticated {
				return invalid, false
			}
		case "session_inactive", "tls_required", "authentication_failed":
			if !scoped || event.Authenticated {
				return invalid, false
			}
		case "path_rejected", "request_limit", "method_rejected", "immutable_object", "lock_not_fresh",
			"object_size", "invalid_body", "content_hash_mismatch", "object_exists", "lock_not_owned":
			if !scoped || !event.Authenticated {
				return invalid, false
			}
		default:
			return invalid, false
		}
	default:
		return invalid, false
	}
	audit := domain.AuditEvent{ActorType: domain.ActorSystem, Action: action, ResourceType: "GATEWAY",
		Result: result, ReasonCode: strings.ToUpper(event.Reason)}
	if scoped {
		audit.ResourceType, audit.ResourceID = "REPOSITORY", event.Binding.RepositoryID
		// These are trusted ROUTE/session context, never the claimed identity of
		// an unauthenticated caller. OperationID is not a durable Backup Operation.
		audit.Changes, _ = json.Marshal(struct {
			HostID        uuid.UUID `json:"route_host_id"`
			GatewayID     uuid.UUID `json:"gateway_id"`
			SessionID     uuid.UUID `json:"session_id"`
			Authenticated bool      `json:"authenticated"`
		}{event.Binding.HostID, event.Binding.GatewayID, event.Binding.OperationID, event.Authenticated})
	}
	return audit, true
}
