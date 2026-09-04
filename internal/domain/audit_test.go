package domain

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestVerifyAuditChainDetectsMutation(t *testing.T) {
	event := AuditEvent{
		ID:           uuid.MustParse("0198f1da-2c57-7d3b-9c92-6e2f05293643"),
		OccurredAt:   time.Date(2026, 9, 3, 7, 0, 0, 0, time.UTC),
		ActorType:    ActorSystem,
		Action:       "AUTH_LOGIN",
		ResourceType: "SESSION",
		RequestID:    uuid.MustParse("0198f1da-2c57-7d3b-9c92-6e2f05293644"),
		Result:       AuditDenied,
		ReasonCode:   "INVALID_CREDENTIALS",
		Changes:      json.RawMessage("{\"attempted\":true}"),
	}
	hash, err := ComputeAuditHash(event, nil)
	if err != nil {
		t.Fatal(err)
	}
	event.EventHash = hash
	if err := VerifyAuditChain([]AuditEvent{event}); err != nil {
		t.Fatalf("valid chain rejected: %v", err)
	}

	event.ReasonCode = "TAMPERED"
	if err := VerifyAuditChain([]AuditEvent{event}); err == nil {
		t.Fatal("modified audit event was not detected")
	}
}
