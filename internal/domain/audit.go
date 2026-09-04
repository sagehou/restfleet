package domain

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
)

const (
	ActorUser   = "USER"
	ActorAgent  = "AGENT"
	ActorSystem = "SYSTEM"

	AuditSuccess = "SUCCESS"
	AuditDenied  = "DENIED"
	AuditFailure = "FAILURE"
)

// AuditEvent is the append-only security event stored by the control plane.
type AuditEvent struct {
	Sequence     int64
	ID           uuid.UUID
	OccurredAt   time.Time
	ActorType    string
	ActorID      uuid.UUID
	Action       string
	ResourceType string
	ResourceID   uuid.UUID
	RequestID    uuid.UUID
	SourceIPHash []byte
	Result       string
	ReasonCode   string
	Changes      json.RawMessage
	PreviousHash []byte
	EventHash    []byte
}

type canonicalAuditEvent struct {
	ID           string          `json:"id"`
	OccurredAt   string          `json:"occurred_at"`
	ActorType    string          `json:"actor_type"`
	ActorID      string          `json:"actor_id,omitempty"`
	Action       string          `json:"action"`
	ResourceType string          `json:"resource_type"`
	ResourceID   string          `json:"resource_id,omitempty"`
	RequestID    string          `json:"request_id"`
	SourceIPHash string          `json:"source_ip_hash,omitempty"`
	Result       string          `json:"result"`
	ReasonCode   string          `json:"reason_code"`
	Changes      json.RawMessage `json:"changes"`
}

func canonicalAudit(event AuditEvent) ([]byte, error) {
	changes := event.Changes
	if len(changes) == 0 {
		changes = json.RawMessage("{}")
	}
	var value any
	if err := json.Unmarshal(changes, &value); err != nil {
		return nil, err
	}
	changes, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}

	actorID := ""
	if event.ActorID != uuid.Nil {
		actorID = event.ActorID.String()
	}
	resourceID := ""
	if event.ResourceID != uuid.Nil {
		resourceID = event.ResourceID.String()
	}
	return json.Marshal(canonicalAuditEvent{
		ID:           event.ID.String(),
		OccurredAt:   event.OccurredAt.UTC().Format(time.RFC3339Nano),
		ActorType:    event.ActorType,
		ActorID:      actorID,
		Action:       event.Action,
		ResourceType: event.ResourceType,
		ResourceID:   resourceID,
		RequestID:    event.RequestID.String(),
		SourceIPHash: hex.EncodeToString(event.SourceIPHash),
		Result:       event.Result,
		ReasonCode:   event.ReasonCode,
		Changes:      changes,
	})
}

// ComputeAuditHash returns SHA-256(canonical event without hashes || previous hash).
func ComputeAuditHash(event AuditEvent, previous []byte) ([]byte, error) {
	canonical, err := canonicalAudit(event)
	if err != nil {
		return nil, err
	}
	sum := sha256.New()
	_, _ = sum.Write(canonical)
	_, _ = sum.Write(previous)
	return sum.Sum(nil), nil
}

// VerifyAuditChain detects a reordered, removed, or modified event in sequence order.
func VerifyAuditChain(events []AuditEvent) error {
	var previous []byte
	for _, event := range events {
		if !bytes.Equal(event.PreviousHash, previous) {
			return errors.New("audit previous hash mismatch")
		}
		expected, err := ComputeAuditHash(event, previous)
		if err != nil {
			return err
		}
		if !bytes.Equal(event.EventHash, expected) {
			return errors.New("audit event hash mismatch")
		}
		previous = event.EventHash
	}
	return nil
}
