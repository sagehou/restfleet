package agent

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/sagehou/restfleet/internal/agent/schedule"
	"github.com/sagehou/restfleet/internal/domain"
)

// OperationOutcome is what an executor reports back for one run.
type OperationOutcome struct {
	Status     string
	ExitCode   int
	ErrorCode  string
	SnapshotID string
	FilesDone  int64
	TotalFiles int64
	BytesDone  int64
	TotalBytes int64
}

// PlanExecutor runs one scheduled operation. Implementations own Restic and the
// Gateway session; the loop owns scheduling state.
type PlanExecutor interface {
	Execute(ctx context.Context, plan domain.PlanSchedule, operation uuid.UUID, scheduled, deadline time.Time) OperationOutcome
}

// RunLocalScheduler blocks until ctx ends. It persists next_run_at before each
// run so a crash cannot replay or skip a slot, then executes due plans.
func (s *State) RunLocalScheduler(ctx context.Context, interval time.Duration, executor PlanExecutor) error {
	if executor == nil {
		return errors.New("local scheduler requires an executor")
	}
	if interval <= 0 {
		interval = 30 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		s.tickScheduler(ctx, executor)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (s *State) tickScheduler(ctx context.Context, executor PlanExecutor) {
	desired, ok, err := s.ActiveDesiredState()
	if err != nil || !ok {
		return
	}
	running, err := s.RunningOperations()
	if err != nil {
		return
	}
	runningByPlan := make(map[uuid.UUID]bool, len(running))
	for _, record := range running {
		runningByPlan[record.PlanID] = true
	}
	now := time.Now().UTC()
	for _, plan := range desired.Plans {
		s.advancePlan(ctx, plan, now, runningByPlan, executor)
	}
}

func (s *State) advancePlan(ctx context.Context, plan domain.PlanSchedule, now time.Time,
	runningByPlan map[uuid.UUID]bool, executor PlanExecutor) {
	scheduler, err := schedule.New(plan.PlanID, schedule.Spec{
		Cron: plan.Cron, Timezone: plan.Timezone,
		JitterSeconds: plan.JitterSeconds, MisfireGraceSeconds: plan.MisfireGraceSeconds,
		ConcurrencyPolicy: plan.ConcurrencyPolicy,
	})
	if err != nil {
		return
	}
	runtime, found, err := s.PlanRuntime(plan.PlanID)
	if err != nil {
		return
	}
	runtime.PlanID = plan.PlanID
	if !found || runtime.NextRunAt.IsZero() {
		if next, ok := scheduler.NextRun(now); ok {
			runtime.NextRunAt = next
		}
		_ = s.PutPlanRuntime(runtime)
		return
	}
	if now.Before(runtime.NextRunAt) {
		return
	}
	scheduled := runtime.NextRunAt
	if next, ok := scheduler.NextRun(scheduled); ok {
		runtime.NextRunAt = next
	} else {
		runtime.NextRunAt = time.Time{}
	}
	runtime.LastScheduledAt = scheduled
	// Advance first: a crash after this point must not replay the slot.
	_ = s.PutPlanRuntime(runtime)

	operationID := schedule.OperationID(plan.PlanID, scheduled)
	if _, exists, err := s.Operation(operationID); err == nil && exists {
		return
	}
	if schedule.DecideMisfire(now, scheduled, plan.MisfireGraceSeconds) == schedule.ReportMissed {
		_ = s.PutOperation(OperationRecord{ID: operationID, PlanID: plan.PlanID, PlanRevision: plan.Revision,
			ConfigHash: plan.ConfigHash, ScheduledAt: scheduled, FinishedAt: now,
			Status: OperationMissed, ErrorCode: "MISFIRE_GRACE_EXCEEDED"})
		return
	}
	deadline := scheduled.Add(24 * time.Hour)
	if deadline.Before(now) {
		deadline = now.Add(24 * time.Hour)
	}
	if schedule.ConcurrencySkip(plan.ConcurrencyPolicy, runningByPlan[plan.PlanID]) {
		_ = s.PutOperation(OperationRecord{ID: operationID, PlanID: plan.PlanID, PlanRevision: plan.Revision,
			ConfigHash: plan.ConfigHash, ScheduledAt: scheduled, FinishedAt: now,
			Status: OperationConcurrencySkipped, ErrorCode: "CONCURRENCY_SKIPPED"})
		return
	}
	record := OperationRecord{ID: operationID, PlanID: plan.PlanID, PlanRevision: plan.Revision,
		ConfigHash: plan.ConfigHash, ScheduledAt: scheduled, StartedAt: now, Status: OperationRunning}
	_ = s.PutOperation(record)
	runtime.LastOperationID = operationID
	_ = s.PutPlanRuntime(runtime)
	runningByPlan[plan.PlanID] = true
	go func() {
		outcome := executor.Execute(ctx, plan, operationID, scheduled, deadline)
		record.Status = outcome.Status
		record.ExitCode = outcome.ExitCode
		record.ErrorCode = outcome.ErrorCode
		record.SnapshotID = outcome.SnapshotID
		record.FilesDone = outcome.FilesDone
		record.TotalFiles = outcome.TotalFiles
		record.BytesDone = outcome.BytesDone
		record.TotalBytes = outcome.TotalBytes
		record.FinishedAt = time.Now().UTC()
		_ = s.PutOperation(record)
	}()
}
