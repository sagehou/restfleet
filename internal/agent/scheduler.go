package agent

import (
	"context"
	"errors"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/sagehou/restfleet/internal/agent/schedule"
	"github.com/sagehou/restfleet/internal/domain"
	bolt "go.etcd.io/bbolt"
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
	Summary *domain.BackupSummary
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
	s.schedulerMu.Lock()
	defer s.schedulerMu.Unlock()
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
		// Manual dispatch shares schedulerMu and persists RUNNING before its
		// goroutine starts, so both admission paths observe the same capacity.
		current, err := s.RunningOperations()
		if err != nil {
			return err
		}
		if uint32(len(current)) >= desired.RuntimePolicy.MaxParallelIOJobs &&
			!schedule.ConcurrencySkip(plan.ConcurrencyPolicy, runningByPlan[plan.PlanID]) {
			continue
		}
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
	changed := runtime.PlanRevision != 0 && (runtime.PlanRevision != plan.Revision || runtime.ConfigHash != plan.ConfigHash)
	runtime.PlanRevision, runtime.ConfigHash = plan.Revision, plan.ConfigHash
	if !found || runtime.NextRunAt.IsZero() || changed {
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
	s.executeOperation(ctx, executor, plan, record, deadline, onError)
	return nil
}

func (s *State) executeOperation(ctx context.Context, executor PlanExecutor, plan domain.PlanSchedule,
	record OperationRecord, deadline time.Time, onError func(error),
) {
	runCtx, endRun := s.BeginOperation(ctx, record.ID)
	s.workers.Add(1)
	go func() {
		defer s.workers.Done()
		defer endRun()
		for {
		outcome := executor.Execute(runCtx, plan, record.ID, record.ScheduledAt, deadline)
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
		record.Summary=outcome.Summary
		record.FinishedAt = time.Now().UTC()
		next,err:=s.finishWithRetry(plan,record,deadline,runCtx.Err()==nil)
		if err!=nil {if onError!=nil{onError(err)};return}
		if next==nil {return}
		record=*next
		deadline=record.Deadline
		nextCtx,endNext:=s.BeginOperation(runCtx,record.ID)
		defer endNext()
		runCtx=nextCtx
		timer:=time.NewTimer(time.Until(record.RetryAt))
		select {
		case <-runCtx.Done():
			timer.Stop()
			record.Status,record.ErrorCode,record.FinishedAt=OperationCanceled,"CANCELED",time.Now().UTC()
			if err:=s.PutOperation(record);err!=nil && onError!=nil{onError(err)}
			return
		case <-timer.C:
		}
		active,found,err:=s.ActiveDesiredState()
		current:=false
		if err==nil && found {for _,candidate:=range active.Plans {current=current || candidate.PlanID==plan.PlanID && candidate.Revision==plan.Revision && candidate.ConfigHash==plan.ConfigHash}}
		if !current || !time.Now().Before(deadline) {
			record.Status,record.ErrorCode,record.FinishedAt=OperationCanceled,"CONFIG_SUPERSEDED",time.Now().UTC()
			if err:=s.PutOperation(record);err!=nil && onError!=nil{onError(err)}
			return
		}
		}
	}()
}

// The failed attempt and its next identity are committed together. Waiting
// keeps the same capacity slot; a crash reports the pending attempt as LOST.
func (s *State) finishWithRetry(plan domain.PlanSchedule,record OperationRecord,deadline time.Time,allowRetry bool) (*OperationRecord,error) {
	var next *OperationRecord
	err:=s.db.Update(func(tx *bolt.Tx) error {
		bucket,err:=tx.CreateBucketIfNotExists([]byte(operationsBucket));if err!=nil{return err}
		payload,err:=json.Marshal(record);if err!=nil{return err}
		if err:=bucket.Put(record.ID[:],payload);err!=nil{return err}
		if !allowRetry || record.Status!=OperationFailed || !domain.RetryableBackupError(record.ErrorCode) || plan.Execution==nil || plan.Execution.RetryPolicy==nil {return nil}
		attempt:=record.Attempt
		if attempt==0 {attempt=1}
		policy:=plan.Execution.RetryPolicy
		if attempt>=policy.Attempts(){return nil}
		retryAt:=time.Now().UTC().Add(time.Duration(policy.Backoff(attempt))*time.Second)
		var desired domain.DesiredState
		if raw:=tx.Bucket([]byte(desiredActiveBucket)).Get([]byte(desiredStateKey));raw==nil || json.Unmarshal(raw,&desired)!=nil{return nil}
		current:=false
		for _,candidate:=range desired.Plans {current=current || candidate.PlanID==plan.PlanID && candidate.Revision==plan.Revision && candidate.ConfigHash==plan.ConfigHash}
		if !current{return nil}
		if runtimes:=tx.Bucket([]byte(planRuntimeBucket));runtimes!=nil {
			var runtime PlanRuntime
			if raw:=runtimes.Get(plan.PlanID[:]);raw!=nil && json.Unmarshal(raw,&runtime)==nil && !runtime.NextRunAt.IsZero() && runtime.NextRunAt.Before(deadline){deadline=runtime.NextRunAt}
		}
		if !retryAt.Before(deadline){return nil}
		id,err:=uuid.NewV7();if err!=nil{return err}
		root:=record.RootOperationID
		if root==uuid.Nil {root=record.ID}
		next=&OperationRecord{ID:id,PlanID:record.PlanID,PlanRevision:record.PlanRevision,ConfigHash:record.ConfigHash,ScheduledAt:retryAt,
			StartedAt:time.Now().UTC(),Status:OperationRunning,ParentOperationID:record.ID,RootOperationID:root,Attempt:attempt+1,RetryAt:retryAt,Deadline:deadline}
		payload,err=json.Marshal(next);if err!=nil{return err}
		return bucket.Put(next.ID[:],payload)
	})
	return next,err
}
