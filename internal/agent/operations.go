package agent

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/google/uuid"
	bolt "go.etcd.io/bbolt"
)

const (
	operationsBucket  = "operations"
	planRuntimeBucket = "plan_runtime"

	OperationRunning            = "RUNNING"
	OperationSucceeded          = "SUCCEEDED"
	OperationSucceededWarnings  = "SUCCEEDED_WITH_WARNINGS"
	OperationFailed             = "FAILED"
	OperationCanceled           = "CANCELED"
	OperationLost               = "LOST"
	OperationMissed             = "MISSED"
	OperationConcurrencySkipped = "CONCURRENCY_SKIPPED"

	// maxPendingResultIDs mirrors the Server Hello bound. Extra results are
	// still replayed after Welcome, so the cap never drops a result.
	maxPendingResultIDs = 100
)

// OperationRecord is the durable local view of one scheduled run. It exists
// before Restic starts so a crash cannot silently lose an execution.
type OperationRecord struct {
	ID             uuid.UUID `json:"id"`
	PlanID         uuid.UUID `json:"plan_id"`
	PlanRevision   int64     `json:"plan_revision"`
	ConfigHash     string    `json:"config_hash"`
	ScheduledAt    time.Time `json:"scheduled_at"`
	StartedAt      time.Time `json:"started_at,omitempty"`
	FinishedAt     time.Time `json:"finished_at,omitempty"`
	Status         string    `json:"status"`
	ExitCode       int       `json:"exit_code,omitempty"`
	ErrorCode      string    `json:"error_code,omitempty"`
	SnapshotID     string    `json:"snapshot_id,omitempty"`
	FilesDone      int64     `json:"files_done"`
	TotalFiles     int64     `json:"total_files"`
	BytesDone      int64     `json:"bytes_done"`
	TotalBytes     int64     `json:"total_bytes"`
	FilesNew       int64     `json:"files_new,omitempty"`
	DataAdded      int64     `json:"data_added,omitempty"`
	Uploaded       bool      `json:"uploaded,omitempty"`
	LastReportedAt time.Time `json:"last_reported_at,omitempty"`
}

// Terminal reports whether the record reached a final state.
func (r OperationRecord) Terminal() bool {
	switch r.Status {
	case OperationSucceeded, OperationSucceededWarnings, OperationFailed,
		OperationCanceled, OperationLost, OperationMissed, OperationConcurrencySkipped:
		return true
	}
	return false
}

// PlanRuntime persists the next scheduled instant so restarts neither skip nor
// duplicate a run.
type PlanRuntime struct {
	PlanID          uuid.UUID `json:"plan_id"`
	LastScheduledAt time.Time `json:"last_scheduled_at,omitempty"`
	NextRunAt       time.Time `json:"next_run_at,omitempty"`
	LastOperationID uuid.UUID `json:"last_operation_id,omitempty"`
}

func (s *State) PutOperation(record OperationRecord) error {
	payload, err := json.Marshal(record)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		bucket, err := tx.CreateBucketIfNotExists([]byte(operationsBucket))
		if err != nil {
			return err
		}
		return bucket.Put(record.ID[:], payload)
	})
}

// BeginOperation creates a cancellable context for one running Restic job. A
// cancel that arrived before the goroutine registered is applied immediately.
func (s *State) BeginOperation(parent context.Context, id uuid.UUID) (context.Context, func()) {
	ctx, cancel := context.WithCancel(parent)
	s.runMu.Lock()
	if s.running == nil {
		s.running = make(map[uuid.UUID]context.CancelFunc)
	}
	if _, canceled := s.canceled[id]; canceled {
		cancel()
	}
	s.running[id] = cancel
	s.runMu.Unlock()
	return ctx, func() {
		cancel()
		s.runMu.Lock()
		delete(s.running, id)
		delete(s.canceled, id)
		s.runMu.Unlock()
	}
}

func (s *State) CancelOperation(id uuid.UUID) bool {
	// A cancel for an already terminal operation must not create a phantom
	// marker that could suppress a later, unrelated execution.
	if record, found, err := s.Operation(id); err == nil && found && record.Terminal() {
		return false
	}
	s.runMu.Lock()
	defer s.runMu.Unlock()
	if cancel, ok := s.running[id]; ok {
		cancel()
		return true
	}
	// A cancel may race with dispatch. Keep a small bounded set so the next
	// registration starts canceled instead of losing the request.
	if s.canceled == nil {
		s.canceled = make(map[uuid.UUID]struct{})
	}
	if len(s.canceled) < 256 {
		s.canceled[id] = struct{}{}
	}
	return false
}

// ConsumeCancel reports and clears a cancel that arrived before its dispatch.
// The caller must not start Restic when it returns true.
func (s *State) ConsumeCancel(id uuid.UUID) bool {
	s.runMu.Lock()
	defer s.runMu.Unlock()
	if _, ok := s.canceled[id]; !ok {
		return false
	}
	delete(s.canceled, id)
	return true
}

func (s *State) MarkOperationUploaded(id uuid.UUID) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte(operationsBucket))
		if bucket == nil {
			return nil
		}
		value := bucket.Get(id[:])
		if value == nil {
			return nil
		}
		var record OperationRecord
		if err := json.Unmarshal(value, &record); err != nil {
			return err
		}
		record.Uploaded = true
		payload, err := json.Marshal(record)
		if err != nil {
			return err
		}
		return bucket.Put(id[:], payload)
	})
}

func (s *State) Operation(id uuid.UUID) (OperationRecord, bool, error) {
	var record OperationRecord
	found := false
	err := s.db.View(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte(operationsBucket))
		if bucket == nil {
			return nil
		}
		value := bucket.Get(id[:])
		if value == nil {
			return nil
		}
		found = true
		return json.Unmarshal(value, &record)
	})
	return record, found, err
}

// RunningOperations lists non-terminal runs. The set is bounded by one entry
// per plan under FORBID, so a full scan is acceptable.
func (s *State) RunningOperations() ([]OperationRecord, error) {
	items := make([]OperationRecord, 0)
	err := s.db.View(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte(operationsBucket))
		if bucket == nil {
			return nil
		}
		return bucket.ForEach(func(_, value []byte) error {
			var record OperationRecord
			if err := json.Unmarshal(value, &record); err != nil {
				return err
			}
			if record.Status == OperationRunning {
				items = append(items, record)
			}
			return nil
		})
	})
	return items, err
}

func (s *State) PlanRuntime(planID uuid.UUID) (PlanRuntime, bool, error) {
	var runtime PlanRuntime
	found := false
	err := s.db.View(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte(planRuntimeBucket))
		if bucket == nil {
			return nil
		}
		value := bucket.Get(planID[:])
		if value == nil {
			return nil
		}
		found = true
		return json.Unmarshal(value, &runtime)
	})
	return runtime, found, err
}

func (s *State) PutPlanRuntime(runtime PlanRuntime) error {
	if runtime.PlanID == uuid.Nil {
		return errors.New("plan runtime requires a plan id")
	}
	payload, err := json.Marshal(runtime)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		bucket, err := tx.CreateBucketIfNotExists([]byte(planRuntimeBucket))
		if err != nil {
			return err
		}
		return bucket.Put(runtime.PlanID[:], payload)
	})
}

// ReportableOperations returns terminal results that still need upload and
// running records whose progress is older than the throttling interval.
func (s *State) ReportableOperations(now time.Time, progressInterval time.Duration) ([]OperationRecord, error) {
	items := make([]OperationRecord, 0)
	err := s.db.View(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte(operationsBucket))
		if bucket == nil {
			return nil
		}
		return bucket.ForEach(func(_, value []byte) error {
			var record OperationRecord
			if err := json.Unmarshal(value, &record); err != nil {
				return err
			}
			switch {
			case record.Terminal() && !record.Uploaded:
				items = append(items, record)
			case record.Status == OperationRunning && now.Sub(record.LastReportedAt) >= progressInterval:
				items = append(items, record)
			}
			return nil
		})
	})
	return items, err
}

// PendingResultIDs lists stored terminal results the Server has not confirmed.
// The Hello field is advisory and bounded; results beyond the cap are replayed
// after Welcome until the Server acknowledges them.
func (s *State) PendingResultIDs(limit int) ([]string, error) {
	ids := make([]string, 0)
	err := s.db.View(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte(operationsBucket))
		if bucket == nil {
			return nil
		}
		return bucket.ForEach(func(_, value []byte) error {
			var record OperationRecord
			if err := json.Unmarshal(value, &record); err != nil {
				return err
			}
			if record.Terminal() && !record.Uploaded {
				ids = append(ids, record.ID.String())
			}
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(ids)
	if limit > 0 && len(ids) > limit {
		ids = ids[:limit]
	}
	return ids, nil
}

// RecoverRunningOperations closes records left RUNNING by a previous Agent
// process. The new process owns no Restic child for them, so leaving them
// RUNNING would block FORBID scheduling forever. Recovery never fabricates
// success; the result is reported as LOST for Server-side reconciliation.
func (s *State) RecoverRunningOperations(now time.Time) (int, error) {
	recovered := 0
	err := s.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte(operationsBucket))
		if bucket == nil {
			return nil
		}
		keys := make([][]byte, 0)
		records := make([]OperationRecord, 0)
		if err := bucket.ForEach(func(key, value []byte) error {
			var record OperationRecord
			if err := json.Unmarshal(value, &record); err != nil {
				return err
			}
			if record.Status != OperationRunning {
				return nil
			}
			record.Status = OperationLost
			record.ErrorCode = "LOCAL_OPERATION_RECOVERED"
			record.FinishedAt = now
			record.Uploaded = false
			keys = append(keys, append([]byte(nil), key...))
			records = append(records, record)
			return nil
		}); err != nil {
			return err
		}
		for i, key := range keys {
			payload, err := json.Marshal(records[i])
			if err != nil {
				return err
			}
			if err := bucket.Put(key, payload); err != nil {
				return err
			}
		}
		recovered = len(keys)
		return nil
	})
	return recovered, err
}
