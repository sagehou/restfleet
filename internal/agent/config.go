package agent

import (
	"encoding/json"
	"errors"

	"github.com/google/uuid"
	bolt "go.etcd.io/bbolt"

	agentv1 "github.com/sagehou/restfleet/api/proto/gen/go/restfleet/agent/v1"
	"github.com/sagehou/restfleet/internal/domain"
)

const (
	desiredStateKey = "current"
	configResultKey = "config_result"
)

type PendingConfigResult struct {
	Revision   int64  `json:"revision"`
	ConfigHash string `json:"config_hash,omitempty"`
	Accepted   bool   `json:"accepted"`
	ErrorCode  string `json:"error_code,omitempty"`
	FieldPath  string `json:"field_path,omitempty"`
}

func (s *State) AcceptedRevision() (int64, error) {
	state, ok, err := s.ActiveDesiredState()
	if err != nil || !ok {
		return 0, err
	}
	return state.Revision, nil
}

func (s *State) ActiveDesiredState() (domain.DesiredState, bool, error) {
	var state domain.DesiredState
	found := false
	err := s.db.View(func(tx *bolt.Tx) error {
		value := tx.Bucket([]byte(desiredActiveBucket)).Get([]byte(desiredStateKey))
		if value == nil {
			return nil
		}
		found = true
		return json.Unmarshal(value, &state)
	})
	return state, found, err
}

func (s *State) ApplyDesiredState(
	agentID uuid.UUID,
	snapshot *agentv1.DesiredStateSnapshot,
) (PendingConfigResult, error) {
	state, err := desiredStateFromProto(agentID, snapshot)
	if err != nil {
		result := rejectedConfigResult(snapshot, err)
		if result.Revision < 1 {
			return PendingConfigResult{}, err
		}
		return result, s.storePendingConfigResult(result)
	}
	active, found, err := s.ActiveDesiredState()
	if err != nil {
		return PendingConfigResult{}, err
	}
	if found && state.Revision < active.Revision {
		err := &domain.DesiredStateError{Code: "REVISION_ROLLBACK", FieldPath: "revision"}
		result := rejectedConfigResult(snapshot, err)
		return result, s.storePendingConfigResult(result)
	}
	if found && state.Revision == active.Revision {
		if state.ConfigHash != active.ConfigHash {
			err := &domain.DesiredStateError{Code: "REVISION_CONFLICT", FieldPath: "config_hash"}
			result := rejectedConfigResult(snapshot, err)
			return result, s.storePendingConfigResult(result)
		}
		result := acceptedConfigResult(state)
		return result, s.storePendingConfigResult(result)
	}
	encoded, err := json.Marshal(state)
	if err != nil {
		return PendingConfigResult{}, err
	}
	if err := s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(desiredStagingBucket)).Put([]byte(desiredStateKey), encoded)
	}); err != nil {
		return PendingConfigResult{}, err
	}
	result := acceptedConfigResult(state)
	resultJSON, err := json.Marshal(result)
	if err != nil {
		return PendingConfigResult{}, err
	}
	if err := s.db.Update(func(tx *bolt.Tx) error {
		if err := tx.Bucket([]byte(desiredActiveBucket)).Put([]byte(desiredStateKey), encoded); err != nil {
			return err
		}
		if err := tx.Bucket([]byte(outboundBucket)).Put([]byte(configResultKey), resultJSON); err != nil {
			return err
		}
		return tx.Bucket([]byte(desiredStagingBucket)).Delete([]byte(desiredStateKey))
	}); err != nil {
		return PendingConfigResult{}, err
	}
	return result, nil
}

func (s *State) PendingConfigResult() (PendingConfigResult, bool, error) {
	var result PendingConfigResult
	found := false
	err := s.db.View(func(tx *bolt.Tx) error {
		value := tx.Bucket([]byte(outboundBucket)).Get([]byte(configResultKey))
		if value == nil {
			return nil
		}
		found = true
		return json.Unmarshal(value, &result)
	})
	return result, found, err
}

func (s *State) ClearPendingConfigResult(revision int64) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte(outboundBucket))
		value := bucket.Get([]byte(configResultKey))
		if value == nil {
			return nil
		}
		var result PendingConfigResult
		if err := json.Unmarshal(value, &result); err != nil {
			return err
		}
		if result.Revision != revision {
			return nil
		}
		return bucket.Delete([]byte(configResultKey))
	})
}

func (s *State) storePendingConfigResult(result PendingConfigResult) error {
	encoded, err := json.Marshal(result)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(outboundBucket)).Put([]byte(configResultKey), encoded)
	})
}

func desiredStateFromProto(
	agentID uuid.UUID,
	snapshot *agentv1.DesiredStateSnapshot,
) (domain.DesiredState, error) {
	if snapshot == nil || snapshot.GetGeneratedAt() == nil ||
		snapshot.GetGeneratedAt().CheckValid() != nil || snapshot.GetRuntimePolicy() == nil {
		return domain.DesiredState{}, &domain.DesiredStateError{
			Code: "SNAPSHOT_INVALID", FieldPath: "",
		}
	}
	state := domain.DesiredState{
		AgentID: agentID, Revision: snapshot.GetRevision(),
		GeneratedAt: snapshot.GetGeneratedAt().AsTime().UTC(),
		ConfigHash:  snapshot.GetConfigHash(),
		RuntimePolicy: domain.RuntimePolicy{
			MaxParallelIOJobs: snapshot.GetRuntimePolicy().GetMaxParallelIoJobs(),
			LogLimitBytes:     snapshot.GetRuntimePolicy().GetLogLimitBytes(),
		},
	}
	return state, state.Validate()
}

func acceptedConfigResult(state domain.DesiredState) PendingConfigResult {
	return PendingConfigResult{
		Revision: state.Revision, ConfigHash: state.ConfigHash, Accepted: true,
	}
}

func rejectedConfigResult(
	snapshot *agentv1.DesiredStateSnapshot,
	err error,
) PendingConfigResult {
	configError := domain.ConfigError(err)
	result := PendingConfigResult{
		ErrorCode: configError.Code, FieldPath: configError.FieldPath,
	}
	if snapshot != nil {
		result.Revision = snapshot.GetRevision()
	}
	return result
}

func (r PendingConfigResult) Validate() error {
	if r.Revision < 1 {
		return errors.New("pending config result is invalid")
	}
	if r.Accepted && r.ConfigHash == "" {
		return errors.New("pending config result is invalid")
	}
	if !r.Accepted && r.ErrorCode == "" {
		return errors.New("pending config result is invalid")
	}
	return nil
}
