package agent

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sagehou/restfleet/internal/domain"
)

func TestRetryCreatesNewDurableIdentityAndRespectsWindow(t *testing.T) {
	state, err := OpenState(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	plan := domain.PlanSchedule{PlanID: uuid.Must(uuid.NewV7()), Revision: 1, Name: "retry", HostID: uuid.Must(uuid.NewV7()), RepositoryID: uuid.Must(uuid.NewV7()), Cron: "0 * * * *", Timezone: "UTC", Paths: []string{"/data"}, ConfigHash: "retry-config",
		Execution: &domain.BackupExecution{RetryPolicy: &domain.BackupRetryPolicy{MaxAttempts: 3, InitialBackoffSeconds: 1, MaxBackoffSeconds: 2}}}
	agentID := uuid.Must(uuid.NewV7())
	if result, err := state.ApplyDesiredState(agentID, desiredSnapshot(t, agentID, 1, plan)); err != nil || !result.Accepted {
		t.Fatalf("config: %+v %v", result, err)
	}
	deadline := time.Now().UTC().Add(time.Minute)
	if err := state.PutPlanRuntime(PlanRuntime{PlanID: plan.PlanID, NextRunAt: deadline}); err != nil {
		t.Fatal(err)
	}
	failed := OperationRecord{ID: uuid.Must(uuid.NewV7()), PlanID: plan.PlanID, PlanRevision: 1, ConfigHash: plan.ConfigHash, Status: OperationFailed, ErrorCode: "REPOSITORY_LOCKED", FinishedAt: time.Now().UTC()}
	child, err := state.finishWithRetry(plan, failed, deadline.Add(time.Hour), true)
	if err != nil || child == nil {
		t.Fatalf("retry missing: %+v %v", child, err)
	}
	if child.ID == failed.ID || child.ParentOperationID != failed.ID || child.RootOperationID != failed.ID || child.Attempt != 2 || !child.Deadline.Equal(deadline) {
		t.Fatalf("retry identity/window: %+v", child)
	}
	stored, found, err := state.Operation(child.ID)
	if err != nil || !found || stored.Status != OperationRunning {
		t.Fatalf("retry not durable: %+v %v", stored, err)
	}
	beforeAck, err := state.ReportableOperations(time.Now().UTC(), 0)
	if err != nil || len(beforeAck) != 1 || beforeAck[0].ID != failed.ID {
		t.Fatalf("child preceded parent ACK: %+v %v", beforeAck, err)
	}
	if err := state.MarkOperationUploaded(failed.ID); err != nil {
		t.Fatal(err)
	}
	afterAck, err := state.ReportableOperations(time.Now().UTC(), 0)
	if err != nil || len(afterAck) != 1 || afterAck[0].ID != child.ID {
		t.Fatalf("child not released by parent ACK: %+v %v", afterAck, err)
	}
	for _, code := range []string{"GATEWAY_UNAVAILABLE", "GATEWAY_RATE_LIMITED"} {
		failed.ID, failed.ErrorCode = uuid.Must(uuid.NewV7()), code
		if next, err := state.finishWithRetry(plan, failed, deadline, true); err != nil || next == nil || next.ParentOperationID != failed.ID || next.Attempt != 2 || !next.Deadline.Equal(deadline) {
			t.Fatalf("verified transient failure lost durable retry: %s %+v %v", code, next, err)
		}
	}
	for _, code := range []string{"WRONG_REPOSITORY_PASSWORD", "CONFIG_INVALID", "GATEWAY_CLEANUP_UNCONFIRMED", "GATEWAY_SESSION_FAILED", "INVALID_ENGINE_OUTPUT", "HTTP_429", "provider-secret-canary"} {
		failed.ErrorCode = code
		if next, err := state.finishWithRetry(plan, failed, deadline, true); err != nil || next != nil {
			t.Fatalf("permanent/uncertain failure retried: %s %v", code, err)
		}
	}
	failed.ErrorCode = "REPOSITORY_LOCKED"
	if next, err := state.finishWithRetry(plan, failed, time.Now().Add(500*time.Millisecond), true); err != nil || next != nil {
		t.Fatalf("retry exceeded deadline: %+v %v", next, err)
	}
	if result, err := state.ApplyDesiredState(agentID, desiredSnapshot(t, agentID, 2)); err != nil || !result.Accepted {
		t.Fatal("pause failed")
	}
	if next, err := state.finishWithRetry(plan, failed, deadline, true); err != nil || next != nil {
		t.Fatalf("paused plan retried: %+v %v", next, err)
	}
}
