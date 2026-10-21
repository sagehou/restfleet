package server

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/sagehou/restfleet/internal/domain"
)

func TestValidPasswordLengthUsesUnicodeCodePoints(t *testing.T) {
	if validPasswordLength("short", 12) {
		t.Fatal("password below the minimum was accepted")
	}

	valid := strings.Repeat("界", maxPasswordRunes)
	if !validPasswordLength(valid, 1) {
		t.Fatal("valid Unicode password at the contract limit was rejected")
	}

	if validPasswordLength(valid+"界", 1) || validPasswordLength(string([]byte{0xff}), 1) {
		t.Fatal("password beyond the contract or UTF-8 limit was accepted")
	}
}

type operationValidationStore struct {
	Store

	progress, result bool
}

func (s *operationValidationStore) UpsertOperationProgress(context.Context, domain.BackupOperationProgress) error {
	s.progress = true
	return nil
}

func (s *operationValidationStore) UpsertOperationResult(context.Context, domain.BackupOperationResult) error {
	s.result = true
	return nil
}

func TestOperationReportsAcceptScheduledUUIDv5(t *testing.T) {
	store := &operationValidationStore{}
	control := &ControlPlane{store: store}
	scheduled := uuid.NewSHA1(uuid.NameSpaceOID, []byte("plan|2026-10-21T00:00:00Z"))
	planID, agentID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())

	if err := control.RecordOperationProgress(context.Background(), domain.BackupOperationProgress{
		OperationID: scheduled, PlanID: planID, AgentID: agentID,
	}); err != nil || !store.progress {
		t.Fatalf("scheduled progress rejected: %v", err)
	}
	if err := control.RecordOperationResult(context.Background(), domain.BackupOperationResult{
		OperationID: scheduled, PlanID: planID, AgentID: agentID, Status: "SUCCEEDED",
	}); err != nil || !store.result {
		t.Fatalf("scheduled result rejected: %v", err)
	}

	random := uuid.New()
	if err := control.RecordOperationProgress(context.Background(), domain.BackupOperationProgress{
		OperationID: random, PlanID: planID, AgentID: agentID,
	}); err == nil {
		t.Fatal("random operation ID accepted")
	}
	if err := control.RecordOperationResult(context.Background(), domain.BackupOperationResult{
		OperationID: random, PlanID: planID, AgentID: agentID, Status: "SUCCEEDED",
	}); err == nil {
		t.Fatal("random operation ID accepted for result")
	}
	if err := control.RecordOperationProgress(context.Background(), domain.BackupOperationProgress{
		OperationID: scheduled, PlanID: random, AgentID: agentID,
	}); err == nil {
		t.Fatal("non-v7 plan ID accepted")
	}
}
