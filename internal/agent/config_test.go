package agent

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"

	agentv1 "github.com/sagehou/restfleet/api/proto/gen/go/restfleet/agent/v1"
	"github.com/sagehou/restfleet/internal/domain"
)

func TestDesiredStateRejectDoesNotReplaceLastGood(t *testing.T) {
	state, err := OpenState(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	agentID := uuid.Must(uuid.NewV7())
	valid := desiredSnapshot(t, agentID, 1)
	result, err := state.ApplyDesiredState(agentID, valid)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Accepted || result.Revision != 1 {
		t.Fatalf("accepted result = %+v", result)
	}
	invalid := desiredSnapshot(t, agentID, 2)
	invalid.RuntimePolicy.MaxParallelIoJobs = 0
	result, err = state.ApplyDesiredState(agentID, invalid)
	if err != nil {
		t.Fatal(err)
	}
	if result.Accepted || result.ErrorCode != "MAX_PARALLEL_IO_JOBS_INVALID" {
		t.Fatalf("rejected result = %+v", result)
	}
	active, ok, err := state.ActiveDesiredState()
	if err != nil {
		t.Fatal(err)
	}
	if !ok || active.Revision != 1 || active.ConfigHash != valid.ConfigHash {
		t.Fatalf("last good desired state replaced: %+v", active)
	}
}

func TestPendingConfigResultIsBoundedAndDurable(t *testing.T) {
	directory := t.TempDir()
	state, err := OpenState(directory)
	if err != nil {
		t.Fatal(err)
	}
	agentID := uuid.Must(uuid.NewV7())
	for revision := int64(1); revision <= 10; revision++ {
		snapshot := desiredSnapshot(t, agentID, revision)
		if _, err := state.ApplyDesiredState(agentID, snapshot); err != nil {
			t.Fatal(err)
		}
	}
	if err := state.Close(); err != nil {
		t.Fatal(err)
	}
	state, err = OpenState(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	pending, ok, err := state.PendingConfigResult()
	if err != nil {
		t.Fatal(err)
	}
	if !ok || !pending.Accepted || pending.Revision != 10 {
		t.Fatalf("pending result = %+v, found = %v", pending, ok)
	}
	if err := state.ClearPendingConfigResult(9); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := state.PendingConfigResult(); err != nil || !ok {
		t.Fatalf("newer result was cleared: found = %v, err = %v", ok, err)
	}
	if err := state.ClearPendingConfigResult(10); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := state.PendingConfigResult(); err != nil || ok {
		t.Fatalf("pending result was not cleared: found = %v, err = %v", ok, err)
	}
}

func desiredSnapshot(t *testing.T, agentID uuid.UUID, revision int64) *agentv1.DesiredStateSnapshot {
	t.Helper()
	state, err := domain.NewDefaultDesiredState(agentID, revision, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	return &agentv1.DesiredStateSnapshot{
		Revision: state.Revision, GeneratedAt: timestamppb.New(state.GeneratedAt),
		ConfigHash: state.ConfigHash,
		RuntimePolicy: &agentv1.RuntimePolicy{
			MaxParallelIoJobs: state.RuntimePolicy.MaxParallelIOJobs,
			LogLimitBytes:     state.RuntimePolicy.LogLimitBytes,
		},
	}
}
