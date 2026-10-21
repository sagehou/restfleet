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
