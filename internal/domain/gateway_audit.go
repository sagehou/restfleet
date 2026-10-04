package domain

import (
	"encoding/json"
	"github.com/google/uuid"
	"strings"
)

// GatewayBinding is trusted route context, never a client claim.
type GatewayBinding struct {
	HostID       uuid.UUID `json:"host_id"`
	RepositoryID uuid.UUID `json:"repository_id"`
	GatewayID    uuid.UUID `json:"gateway_id"`
	OperationID  uuid.UUID `json:"session_id"`
}

// GatewayEvent contains fixed classifications only; no request or provider data.
type GatewayEvent struct {
	Binding       GatewayBinding `json:"binding"`
	Authenticated bool           `json:"authenticated"`
	Action        string         `json:"action"`
	Reason        string         `json:"reason"`
}

func GatewayAudit(event GatewayEvent) (AuditEvent, bool) {
	invalid := AuditEvent{ActorType: ActorSystem, Action: "GATEWAY_EVENT_REJECTED",
		ResourceType: "GATEWAY", Result: AuditDenied, ReasonCode: "INVALID_EVENT"}
	scoped := event.Binding != (GatewayBinding{})
	if scoped {
		for _, id := range []uuid.UUID{event.Binding.HostID, event.Binding.RepositoryID, event.Binding.GatewayID, event.Binding.OperationID} {
			if id.Version() != 7 || id.Variant() != uuid.RFC4122 {
				return invalid, false
			}
		}
	}
	action, result := "", AuditSuccess
	switch event.Action {
	case "channel_denied":
		if scoped || event.Authenticated {
			return invalid, false
		}
		result = AuditDenied
		switch event.Reason {
		case "material_rejected":
			action = "GATEWAY_MATERIAL_DELIVERY_DENIED"
		case "authority_rejected":
			action = "GATEWAY_AUTHORIZATION_DELIVERY_DENIED"
		case "session_rejected":
			action = "GATEWAY_SESSION_DELIVERY_DENIED"
		default:
			return invalid, false
		}
	case "event_rejected":
		if scoped || event.Authenticated || event.Reason != "invalid_event" {
			return invalid, false
		}
		return invalid, true
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
		action, result = "GATEWAY_REQUEST_DENIED", AuditDenied
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
	audit := AuditEvent{ActorType: ActorSystem, Action: action, ResourceType: "GATEWAY",
		Result: result, ReasonCode: strings.ToUpper(event.Reason)}
	if event.Action == "channel_denied" {
		audit.ReasonCode = "REJECTED"
	}
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
