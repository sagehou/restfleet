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
	FilesNew   int64
	DataAdded  int64
}

// PlanExecutor runs one scheduled operation. Implementations own Restic and the
// Gateway session; the loop owns scheduling state.
type PlanExecutor interface {
	Execute(ctx context.Context, plan domain.PlanSchedule, operation uuid.UUID, scheduled, deadline time.Time) OperationOutcome
}

// RunLocalScheduler blocks until ctx ends. It persists next_run_at before each
// run so a crash cannot replay or skip a slot, then executes due plans.
func (s *State) RunLocalScheduler(ctx context.Context, interval time.Duration, executor PlanExecutor, onError func(error)) error {
	if executor == nil {
		return errors.New("local scheduler requires an executor")
	}
	if interval <= 0 {
		interval = 30 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := s.tickScheduler(ctx, executor, onError); err != nil {
			if onError == nil {
				return err
			}
			onError(err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (s *State) tickScheduler(ctx context.Context, executor PlanExecutor, onError func(error)) error {
	desired, ok, err := s.ActiveDesiredState()
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	running, err := s.RunningOperations()
	if err != nil {
		return err
	}
	runningByPlan := make(map[uuid.UUID]bool, len(running))
	for _, record := range running {
		runningByPlan[record.PlanID] = true
	}
	now := time.Now().UTC()
	var firstErr error
	for _, plan := range desired.Plans {
		if err := s.advancePlan(ctx, plan, now, runningByPlan, executor, onError); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (s *State) advancePlan(ctx context.Context, plan domain.PlanSchedule, now time.Time,
	runningByPlan map[uuid.UUID]bool, executor PlanExecutor, onError func(error),
) error {
	scheduler, err := schedule.New(plan.PlanID, schedule.Spec{
		Cron: plan.Cron, Timezone: plan.Timezone,
		JitterSeconds: plan.JitterSeconds, MisfireGraceSeconds: plan.MisfireGraceSeconds,
		ConcurrencyPolicy: plan.ConcurrencyPolicy,
	})
	if err != nil {
		return err
	}
	runtime, found, err := s.PlanRuntime(plan.PlanID)
	if err != nil {
		return err
	}
	runtime.PlanID = plan.PlanID
	if !found || runtime.NextRunAt.IsZero() {
		if next, ok := scheduler.NextRun(now); ok {
			runtime.NextRunAt = next
		}
		return s.PutPlanRuntime(runtime)
	}
	if now.Before(runtime.NextRunAt) {
		return nil
	}
	scheduled := runtime.NextRunAt
	// Compute the next run from the current instant, not from the already
	// jittered scheduled time; otherwise jitter accumulates across runs.
	if next, ok := scheduler.NextRun(now); ok {
		runtime.NextRunAt = next
	} else {
		runtime.NextRunAt = time.Time{}
	}
	runtime.LastScheduledAt = scheduled

	operationID := schedule.OperationID(plan.PlanID, scheduled)
	if _, exists, err := s.Operation(operationID); err != nil {
		return err
	} else if exists {
		// The slot was already recorded; only advance the runtime.
		return s.PutPlanRuntime(runtime)
	}
	if schedule.DecideMisfire(now, scheduled, plan.MisfireGraceSeconds) == schedule.ReportMissed {
		return s.PutOperationWithRuntime(OperationRecord{ID: operationID, PlanID: plan.PlanID, PlanRevision: plan.Revision,
			ConfigHash: plan.ConfigHash, ScheduledAt: scheduled, FinishedAt: now,
			Status: OperationMissed, ErrorCode: "MISFIRE_GRACE_EXCEEDED"}, runtime)
	}
	deadline := scheduled.Add(24 * time.Hour)
	if deadline.Before(now) {
		deadline = now.Add(24 * time.Hour)
	}
	if schedule.ConcurrencySkip(plan.ConcurrencyPolicy, runningByPlan[plan.PlanID]) {
		return s.PutOperationWithRuntime(OperationRecord{ID: operationID, PlanID: plan.PlanID, PlanRevision: plan.Revision,
			ConfigHash: plan.ConfigHash, ScheduledAt: scheduled, FinishedAt: now,
			Status: OperationConcurrencySkipped, ErrorCode: "CONCURRENCY_SKIPPED"}, runtime)
	}
	record := OperationRecord{ID: operationID, PlanID: plan.PlanID, PlanRevision: plan.Revision,
		ConfigHash: plan.ConfigHash, ScheduledAt: scheduled, StartedAt: now, Status: OperationRunning}
	runtime.LastOperationID = operationID
	if err := s.PutOperationWithRuntime(record, runtime); err != nil {
		return err
	}
	runningByPlan[plan.PlanID] = true
	runCtx, endRun := s.BeginOperation(ctx, operationID)
	go func() {
		defer endRun()
		outcome := executor.Execute(runCtx, plan, operationID, scheduled, deadline)
		record.Status = outcome.Status
		record.ExitCode = outcome.ExitCode
		record.ErrorCode = outcome.ErrorCode
		record.SnapshotID = outcome.SnapshotID
		record.FilesDone = outcome.FilesDone
		record.TotalFiles = outcome.TotalFiles
		record.BytesDone = outcome.BytesDone
		record.TotalBytes = outcome.TotalBytes
		record.FilesNew = outcome.FilesNew
		record.DataAdded = outcome.DataAdded
		record.FinishedAt = time.Now().UTC()
		if err := s.PutOperation(record); err != nil && onError != nil {
			onError(err)
		}
	}()
	return nil
}
