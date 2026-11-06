package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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

func TestConfigActivationReconcilesScheduleWithoutLosingRunningOperation(t *testing.T) {
	state, err := OpenState(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	agentID := uuid.Must(uuid.NewV7())
	plan := domain.PlanSchedule{PlanID: uuid.Must(uuid.NewV7()), Revision: 1,
		Name: "config-change", HostID: uuid.Must(uuid.NewV7()), RepositoryID: uuid.Must(uuid.NewV7()),
		Cron: "0 0 * * *", Timezone: "UTC", Paths: []string{"/data"}, ConfigHash: "initial"}
	apply := func(revision int64, plans ...domain.PlanSchedule) {
		t.Helper()
		snapshot := desiredSnapshot(t, agentID, revision, plans...)
		if result, err := state.ApplyDesiredState(agentID, snapshot); err != nil || !result.Accepted {
			t.Fatalf("apply = %+v, %v", result, err)
		}
	}
	apply(1, plan)
	oldNext := time.Now().UTC().Add(8 * time.Hour)
	runID := uuid.Must(uuid.NewV7())
	if err := state.PutPlanRuntime(PlanRuntime{PlanID: plan.PlanID, PlanRevision: 1,
		ConfigHash: plan.ConfigHash, NextRunAt: oldNext, LastOperationID: runID}); err != nil {
		t.Fatal(err)
	}
	if err := state.PutOperation(OperationRecord{ID: runID, PlanID: plan.PlanID, Status: OperationRunning}); err != nil {
		t.Fatal(err)
	}
	plan.Revision, plan.ConfigHash, plan.Cron = 2, "changed", "* * * * *"
	apply(2, plan)
	runtime, _, err := state.PlanRuntime(plan.PlanID)
	if err != nil || runtime.PlanRevision != 2 || runtime.NextRunAt.After(time.Now().Add(time.Minute)) ||
		runtime.LastOperationID != runID || runtime.NextRunAt.Equal(oldNext) {
		t.Fatalf("new configuration retained stale runtime: %+v, %v", runtime, err)
	}
	apply(3, plan)
	unchanged, _, err := state.PlanRuntime(plan.PlanID)
	if err != nil || !unchanged.NextRunAt.Equal(runtime.NextRunAt) {
		t.Fatalf("unchanged plan lost its slot: %+v, %v", unchanged, err)
	}
	apply(4)
	if runs, err := state.NextRuns(100); err != nil || len(runs) != 0 {
		t.Fatalf("removed plan still advertised: %+v, %v", runs, err)
	}
	if operation, found, err := state.Operation(runID); err != nil || !found || operation.Status != OperationRunning {
		t.Fatalf("configuration change lost active run: %+v, %v", operation, err)
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

func desiredSnapshot(t *testing.T, agentID uuid.UUID, revision int64, plans ...domain.PlanSchedule) *agentv1.DesiredStateSnapshot {
	t.Helper()
	state, err := domain.NewDefaultDesiredState(agentID, revision, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	state.Plans = plans
	canonical, err := state.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(canonical)
	state.ConfigHash = "sha256:" + hex.EncodeToString(sum[:])
	var protoPlans []*agentv1.PlanSchedule
	for _, plan := range plans {
		var execution []byte
		if plan.Execution != nil {
			execution, err = json.Marshal(plan.Execution)
			if err != nil {
				t.Fatal(err)
			}
		}
		protoPlans = append(protoPlans, &agentv1.PlanSchedule{
			PlanId: plan.PlanID.String(), Revision: plan.Revision, Name: plan.Name,
			HostId: plan.HostID.String(), RepositoryId: plan.RepositoryID.String(),
			Cron: plan.Cron, Timezone: plan.Timezone, Paths: plan.Paths, ConfigHash: plan.ConfigHash,
			JitterSeconds: int32(plan.JitterSeconds), MisfireGraceSeconds: int32(plan.MisfireGraceSeconds),
			ConcurrencyPolicy:   plan.ConcurrencyPolicy,
			ExecutionConfigJson: execution, ExcludePatterns: plan.ExcludePatterns, ExcludeCaches: plan.ExcludeCaches, OneFileSystem: plan.OneFileSystem,
		})
	}
	return &agentv1.DesiredStateSnapshot{
		Plans:    protoPlans,
		Revision: state.Revision, GeneratedAt: timestamppb.New(state.GeneratedAt),
		ConfigHash: state.ConfigHash,
		RuntimePolicy: &agentv1.RuntimePolicy{
			MaxParallelIoJobs: state.RuntimePolicy.MaxParallelIOJobs,
			LogLimitBytes:     state.RuntimePolicy.LogLimitBytes,
		},
	}
}

func TestExecutionConfigIsNotSilentlyDropped(t *testing.T) {
	agentID := uuid.Must(uuid.NewV7())
	plan := domain.PlanSchedule{PlanID: uuid.Must(uuid.NewV7()), Revision: 1, Name: "execution", HostID: uuid.Must(uuid.NewV7()), RepositoryID: uuid.Must(uuid.NewV7()), Cron: "0 * * * *", Timezone: "UTC", Paths: []string{"/data"}}
	plan.Execution = &domain.BackupExecution{RetryPolicy: &domain.BackupRetryPolicy{MaxAttempts: 3}, ResourceLimits: &domain.BackupResourceLimits{LimitUploadKiB: 512}}
	snapshot := desiredSnapshot(t, agentID, 1, plan)
	parsed, err := desiredStateFromProto(agentID, snapshot)
	if err != nil || parsed.Plans[0].Execution == nil || parsed.Plans[0].Execution.ResourceLimits.LimitUploadKiB != 512 {
		t.Fatalf("execution config dropped: %v", err)
	}
	for _, raw := range []string{`{"retry_policy":{"unknown":true}}`, `{"resource_limits":{"read_concurrency":100}}`, `null`, `{} {}`} {
		snapshot.Plans[0].ExecutionConfigJson = []byte(raw)
		if _, err := desiredStateFromProto(agentID, snapshot); err == nil {
			t.Fatalf("unsafe execution accepted: %s", raw)
		}
	}
	state, err := OpenState(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	plan.Execution.PreHookIDs = []uuid.UUID{uuid.Must(uuid.NewV7())}
	result, err := state.ApplyDesiredState(agentID, desiredSnapshot(t, agentID, 2, plan))
	if err != nil || result.Accepted || result.ErrorCode != "HOOK_UNAVAILABLE" {
		t.Fatalf("unknown local Hook accepted: %+v %v", result, err)
	}
}
