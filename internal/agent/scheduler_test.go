package agent

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sagehou/restfleet/internal/agent/schedule"
	"github.com/sagehou/restfleet/internal/domain"
)

type schedulerExecutor struct {
	called chan struct{}
}

type blockingSchedulerExecutor struct {
	started chan uuid.UUID
}

func (e *blockingSchedulerExecutor) Execute(ctx context.Context, plan domain.PlanSchedule, _ uuid.UUID, _, _ time.Time) OperationOutcome {
	e.started <- plan.PlanID
	<-ctx.Done()
	return OperationOutcome{Status: OperationCanceled}
}

func TestSchedulerSharesCapacityAcrossPlansAndWaitsForShutdown(t *testing.T) {
	state, err := OpenState(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	plans := make([]domain.PlanSchedule, 2)
	for i := range plans {
		plans[i] = domain.PlanSchedule{PlanID: uuid.Must(uuid.NewV7()), Revision: 1, Name: "capacity",
			HostID: uuid.Must(uuid.NewV7()), RepositoryID: uuid.Must(uuid.NewV7()),
			Cron: "* * * * *", Timezone: "UTC", Paths: []string{"/data"}, MisfireGraceSeconds: 120}
	}
	agentID := uuid.Must(uuid.NewV7())
	if result, err := state.ApplyDesiredState(agentID, desiredSnapshot(t, agentID, 1, plans...)); err != nil || !result.Accepted {
		t.Fatalf("apply = %+v, %v", result, err)
	}
	for _, plan := range plans {
		if err := state.PutPlanRuntime(PlanRuntime{PlanID: plan.PlanID, PlanRevision: 1, NextRunAt: time.Now().Add(-time.Second)}); err != nil {
			t.Fatal(err)
		}
	}
	executor := &blockingSchedulerExecutor{started: make(chan uuid.UUID, 2)}
	if err := state.tickScheduler(context.Background(), executor, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-executor.started:
	case <-time.After(2 * time.Second):
		t.Fatal("no plan started")
	}
	if running, err := state.RunningOperations(); err != nil || len(running) != 1 {
		t.Fatalf("parallel limit exceeded: %+v, %v", running, err)
	}
	if err := state.tickScheduler(context.Background(), executor, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-executor.started:
		t.Fatal("second plan bypassed capacity")
	default:
	}
	if err := state.Close(); err != nil {
		t.Fatal(err)
	}
}

func (e *schedulerExecutor) Execute(context.Context, domain.PlanSchedule, uuid.UUID, time.Time, time.Time) OperationOutcome {
	close(e.called)
	return OperationOutcome{Status: OperationSucceeded}
}

func TestSchedulerUsesCurrentTimeForNextJitteredRun(t *testing.T) {
	state, err := OpenState(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	now := time.Date(2026, 10, 21, 12, 0, 30, 0, time.UTC)
	plan := domain.PlanSchedule{
		PlanID: uuid.Must(uuid.NewV7()), Revision: 1, Name: "scheduler-test",
		HostID: uuid.Must(uuid.NewV7()), RepositoryID: uuid.Must(uuid.NewV7()),
		Cron: "*/5 * * * *", Timezone: "UTC", JitterSeconds: 60, MisfireGraceSeconds: 120,
		ConcurrencyPolicy: "FORBID", Paths: []string{"/tmp"}, ConfigHash: "a",
	}
	scheduled := now.Add(-time.Minute)
	if err := state.PutPlanRuntime(PlanRuntime{PlanID: plan.PlanID, NextRunAt: scheduled}); err != nil {
		t.Fatal(err)
	}
	executor := &schedulerExecutor{called: make(chan struct{})}
	if err := state.advancePlan(context.Background(), plan, now, map[uuid.UUID]bool{}, executor, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-executor.called:
	case <-time.After(2 * time.Second):
		t.Fatal("scheduler did not execute due plan")
	}
	runtime, found, err := state.PlanRuntime(plan.PlanID)
	if err != nil || !found {
		t.Fatal("plan runtime missing")
	}
	clock, err := schedule.New(plan.PlanID, schedule.Spec{
		Cron: plan.Cron, Timezone: plan.Timezone, JitterSeconds: plan.JitterSeconds,
		MisfireGraceSeconds: plan.MisfireGraceSeconds, ConcurrencyPolicy: plan.ConcurrencyPolicy,
	})
	if err != nil {
		t.Fatal(err)
	}
	expected, ok := clock.NextRun(now)
	if !ok || !runtime.NextRunAt.Equal(expected) {
		t.Fatalf("next run = %v, want %v (jitter must start from now, not the previous jittered time)", runtime.NextRunAt, expected)
	}
	operation, exists, err := state.Operation(schedule.OperationID(plan.PlanID, scheduled))
	if err != nil || !exists {
		t.Fatalf("scheduled operation = %+v, %v, %v", operation, exists, err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for operation.Status != OperationSucceeded && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
		operation, exists, err = state.Operation(schedule.OperationID(plan.PlanID, scheduled))
		if err != nil || !exists {
			t.Fatalf("scheduled operation = %+v, %v, %v", operation, exists, err)
		}
	}
	if operation.Status != OperationSucceeded {
		t.Fatalf("operation did not reach terminal state: %+v", operation)
	}
}

func TestSchedulerReturnsPersistenceError(t *testing.T) {
	state, err := OpenState(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := state.Close(); err != nil {
		t.Fatal(err)
	}
	if err := state.tickScheduler(context.Background(), &schedulerExecutor{called: make(chan struct{})}, nil); err == nil {
		t.Fatal("scheduler ignored a closed state store")
	}
}
