package agent

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sagehou/restfleet/internal/domain"
	bolt "go.etcd.io/bbolt"
)

func TestLogsRemainBoundedAcrossAcknowledgementsAndRestart(t *testing.T) {
	directory := t.TempDir()
	state, err := OpenState(directory)
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.Must(uuid.NewV7())
	if err := state.PutOperation(OperationRecord{ID: id, Status: OperationRunning, LastReportedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if err := state.db.Update(func(tx *bolt.Tx) error {
		raw, err := json.Marshal(domain.DesiredState{RuntimePolicy: domain.RuntimePolicy{LogLimitBytes: 64 << 10}})
		if err != nil {
			return err
		}
		return tx.Bucket([]byte(desiredActiveBucket)).Put([]byte(desiredStateKey), raw)
	}); err != nil {
		t.Fatal(err)
	}
	for _, code := range []string{"BACKUP_STARTED", "BACKUP_PROGRESS"} {
		if err := state.operationLog(id, "backup", "INFO", code, 1, 2); err != nil {
			t.Fatal(err)
		}
	}
	chunks, err := state.pendingLogs(time.Now().UTC(), true)
	if err != nil || len(chunks) != 1 {
		t.Fatalf("first chunk: %+v %v", chunks, err)
	}
	original := append([]byte(nil), chunks[0].JSON...)
	if err := state.acknowledgeLogs(id, chunks[0].Last); err == nil {
		t.Fatal("accepted ACK for unsent entries")
	}
	if err := state.markLogsSent(id, chunks[0].Last, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := state.Close(); err != nil {
		t.Fatal(err)
	}
	state, err = OpenState(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	chunks, err = state.pendingLogs(time.Now().UTC(), true)
	if err != nil || len(chunks) != 1 || !bytes.Equal(original, chunks[0].JSON) {
		t.Fatal("restart did not replay exact unacknowledged entries")
	}
	if err := state.acknowledgeLogs(id, chunks[0].Last); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 1500; i++ {
		if err := state.operationLog(id, "backup", "INFO", "BACKUP_PROGRESS", int64(i), int64(i)*100); err != nil {
			t.Fatal(err)
		}
	}
	for _, code := range []string{"ENGINE_READ_ERROR", "BACKUP_SUMMARY", "BACKUP_SUCCEEDED_WITH_WARNINGS"} {
		if err := state.operationLog(id, "backup", "WARN", code, 1500, 150000); err != nil {
			t.Fatal(err)
		}
	}
	var meta localLogState
	if err := state.db.View(func(tx *bolt.Tx) error {
		return json.Unmarshal(tx.Bucket([]byte(logsBucket)).Bucket(id[:]).Get([]byte("meta")), &meta)
	}); err != nil {
		t.Fatal(err)
	}
	if meta.Bytes > 64<<10 || !meta.Truncated {
		t.Fatalf("unbounded log budget: %+v", meta)
	}
	expected := uint64(3)
	codes := map[string]int{}
	for {
		chunks, err = state.pendingLogs(time.Now().UTC(), false)
		if err != nil {
			t.Fatal(err)
		}
		if len(chunks) == 0 {
			break
		}
		chunk := chunks[0]
		if len(chunk.JSON) > domain.MaxBackupLogChunkBytes {
			t.Fatal("oversized chunk")
		}
		var entries []domain.BackupLogEntry
		if err := json.Unmarshal(chunk.JSON, &entries); err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if entry.Sequence != expected {
				t.Fatal("gap after dropping progress")
			}
			expected++
			codes[entry.Code]++
		}
		if err := state.markLogsSent(id, chunk.Last, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
		if err := state.acknowledgeLogs(id, chunk.Last); err != nil {
			t.Fatal(err)
		}
	}
	for _, code := range []string{"LOG_TRUNCATED", "ENGINE_READ_ERROR", "BACKUP_SUMMARY", "BACKUP_SUCCEEDED_WITH_WARNINGS"} {
		if codes[code] != 1 {
			t.Fatalf("critical log %s lost or repeated", code)
		}
	}
	if expected != meta.Next+1 {
		t.Fatal("ACK removed unconfirmed entries")
	}
	if err := state.operationLog(id, "backup", "INFO", "BACKUP_PROGRESS", 2000, 200000); err != nil {
		t.Fatal(err)
	}
	if chunks, err := state.pendingLogs(time.Now().UTC(), true); err != nil || len(chunks) != 0 {
		t.Fatal("ACK reset cumulative byte budget")
	}
	if err := state.operationLog(id, "backup", "ERROR", "secret-canary", 0, 0); err == nil {
		t.Fatal("arbitrary subprocess text accepted")
	}
}

func TestLogsWaitForOperationAdmissionAndTerminalAcknowledgement(t *testing.T) {
	state, err := OpenState(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	id := uuid.Must(uuid.NewV7())
	record := OperationRecord{ID: id, Status: OperationRunning}
	if err := state.PutOperation(record); err != nil {
		t.Fatal(err)
	}
	if err := state.operationLog(id, "backup", "INFO", "BACKUP_STARTED", 0, 0); err != nil {
		t.Fatal(err)
	}
	if chunks, err := state.pendingLogs(time.Now().UTC(), true); err != nil || len(chunks) != 0 {
		t.Fatal("log preceded operation report")
	}
	record.Status = OperationSucceeded
	if err := state.PutOperation(record); err != nil {
		t.Fatal(err)
	}
	if chunks, err := state.pendingLogs(time.Now().UTC(), true); err != nil || len(chunks) != 0 {
		t.Fatal("log preceded terminal ACK")
	}
	if err := state.MarkOperationUploaded(id); err != nil {
		t.Fatal(err)
	}
	if chunks, err := state.pendingLogs(time.Now().UTC(), true); err != nil || len(chunks) != 1 {
		t.Fatal("terminal ACK did not release logs")
	}
}
