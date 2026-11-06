package agent

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestReportWindowPrioritizesResultsAndDoesNotStarveBacklog(t *testing.T) {
	state, err := OpenState(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	now := time.Now().UTC()
	for range maxReportsPerBatch + 7 {
		if err := state.PutOperation(OperationRecord{ID: uuid.Must(uuid.NewV7()), Status: OperationSucceeded}); err != nil {
			t.Fatal(err)
		}
	}
	runningID := uuid.Must(uuid.NewV7())
	if err := state.PutOperation(OperationRecord{ID: runningID, Status: OperationRunning}); err != nil {
		t.Fatal(err)
	}
	first, err := state.ReportableOperations(now, 5*time.Second)
	if err != nil || len(first) != maxReportsPerBatch {
		t.Fatalf("first window: %d %v", len(first), err)
	}
	seen := make(map[uuid.UUID]bool)
	for _, r := range first {
		if !r.Terminal() {
			t.Fatal("progress preceded pending results")
		}
		seen[r.ID] = true
		if err := state.markOperationReported(r.ID, now); err != nil {
			t.Fatal(err)
		}
	}
	second, err := state.ReportableOperations(now.Add(time.Second), 5*time.Second)
	if err != nil || len(second) != 8 {
		t.Fatalf("second window: %d %v", len(second), err)
	}
	for _, r := range second {
		if seen[r.ID] {
			t.Fatal("unacknowledged first window flooded the next window")
		}
	}
	if second[len(second)-1].ID != runningID {
		t.Fatal("running progress not replayed after results")
	}
	for _, r := range second {
		if err := state.markOperationReported(r.ID, now); err != nil {
			t.Fatal(err)
		}
	}
	if records, err := state.ReportableOperations(now.Add(2*time.Second), 5*time.Second); err != nil || len(records) != 0 {
		t.Fatalf("report throttle: %d %v", len(records), err)
	}
	if records, err := state.ReportableOperations(now.Add(2*time.Second), 0); err != nil || len(records) != maxReportsPerBatch {
		t.Fatalf("reconnect replay: %d %v", len(records), err)
	}
}

func TestLateProgressAndReportCannotOverwriteTerminalResult(t *testing.T) {
	state, err := OpenState(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	id := uuid.Must(uuid.NewV7())
	if err := state.PutOperation(OperationRecord{ID: id, Status: OperationSucceeded, SnapshotID: "terminal", FilesDone: 9}); err != nil {
		t.Fatal(err)
	}
	sink := &progressSink{state: state, operation: id, files: 1}
	sink.persist()
	if err := state.markOperationReported(id, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := state.MarkOperationUploaded(id); err != nil {
		t.Fatal(err)
	}
	record, _, err := state.Operation(id)
	if err != nil || record.Status != OperationSucceeded || record.SnapshotID != "terminal" || record.FilesDone != 9 || !record.Uploaded {
		t.Fatalf("terminal record overwritten: %+v %v", record, err)
	}
}
