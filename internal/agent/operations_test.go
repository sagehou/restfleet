package agent

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestCancelBeforeDispatchIsConsumedOnce(t *testing.T) {
	state, err := OpenState(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	operationID := uuid.Must(uuid.NewV7())
	if state.CancelOperation(operationID) {
		t.Fatal("cancel of an unknown operation reported a running process")
	}
	if !state.ConsumeCancel(operationID) {
		t.Fatal("cancel-before-dispatch marker was lost")
	}
	if state.ConsumeCancel(operationID) {
		t.Fatal("cancel marker was consumed twice")
	}
}

func TestBeginOperationAppliesCancelThatArrivedFirst(t *testing.T) {
	state, err := OpenState(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	operationID := uuid.Must(uuid.NewV7())
	state.CancelOperation(operationID)
	ctx, end := state.BeginOperation(context.Background(), operationID)
	defer end()
	select {
	case <-ctx.Done():
	default:
		t.Fatal("a cancel that arrived before registration did not stop the operation")
	}
}

func TestCancelDoesNotSuppressTerminalOperation(t *testing.T) {
	state, err := OpenState(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	operationID := uuid.Must(uuid.NewV7())
	if err := state.PutOperation(OperationRecord{
		ID: operationID, PlanID: uuid.Must(uuid.NewV7()), Status: OperationSucceeded,
		FinishedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if state.CancelOperation(operationID) {
		t.Fatal("terminal operation reported a running process")
	}
	if state.ConsumeCancel(operationID) {
		t.Fatal("terminal operation received a cancel-before-dispatch marker")
	}
}

func TestRecoverRunningOperationsMarksLost(t *testing.T) {
	state, err := OpenState(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	runningID := uuid.Must(uuid.NewV7())
	finishedID := uuid.Must(uuid.NewV7())
	if err := state.PutOperation(OperationRecord{
		ID: runningID, PlanID: uuid.Must(uuid.NewV7()), Status: OperationRunning,
		StartedAt: time.Now().UTC().Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if err := state.PutOperation(OperationRecord{
		ID: finishedID, PlanID: uuid.Must(uuid.NewV7()), Status: OperationSucceeded,
		FinishedAt: time.Now().UTC().Add(-time.Minute), Uploaded: true,
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	recovered, err := state.RecoverRunningOperations(now)
	if err != nil || recovered != 1 {
		t.Fatalf("recovered = %d, %v; want 1", recovered, err)
	}
	record, found, err := state.Operation(runningID)
	if err != nil || !found {
		t.Fatal("recovered operation disappeared")
	}
	if record.Status != OperationLost || record.ErrorCode != "LOCAL_OPERATION_RECOVERED" ||
		record.Uploaded || !record.Terminal() || record.FinishedAt.IsZero() {
		t.Fatalf("recovered record = %+v", record)
	}
	finished, _, err := state.Operation(finishedID)
	if err != nil || finished.Status != OperationSucceeded {
		t.Fatal("terminal record was rewritten by recovery")
	}
	if recovered, err := state.RecoverRunningOperations(now.Add(time.Second)); err != nil || recovered != 0 {
		t.Fatalf("second recovery = %d, %v; want 0", recovered, err)
	}
}

func TestPendingResultIDsBoundAndExcludeUploaded(t *testing.T) {
	state, err := OpenState(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	for i := 0; i < 3; i++ {
		id := uuid.Must(uuid.NewV7())
		if err := state.PutOperation(OperationRecord{
			ID: id, PlanID: uuid.Must(uuid.NewV7()), Status: OperationFailed,
			FinishedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	uploaded := uuid.Must(uuid.NewV7())
	if err := state.PutOperation(OperationRecord{
		ID: uploaded, PlanID: uuid.Must(uuid.NewV7()), Status: OperationSucceeded,
		FinishedAt: time.Now().UTC(), Uploaded: true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := state.PutOperation(OperationRecord{
		ID: uuid.Must(uuid.NewV7()), PlanID: uuid.Must(uuid.NewV7()), Status: OperationRunning,
	}); err != nil {
		t.Fatal(err)
	}
	pending, err := state.PendingResultIDs(2)
	if err != nil || len(pending) != 2 {
		t.Fatalf("bounded pending = %v, %v", pending, err)
	}
	if pending[0] >= pending[1] {
		t.Fatal("pending result IDs are not sorted")
	}
	all, err := state.PendingResultIDs(maxPendingResultIDs)
	if err != nil || len(all) != 3 {
		t.Fatalf("all pending = %v, %v", all, err)
	}
	for _, id := range all {
		if id == uploaded.String() {
			t.Fatal("uploaded result was advertised as pending")
		}
	}
}
