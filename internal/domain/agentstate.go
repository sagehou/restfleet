package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
)

const (
	AgentHealthUnknown  = "UNKNOWN"
	AgentHealthOnline   = "ONLINE"
	AgentHealthDegraded = "DEGRADED"
	AgentHealthOffline  = "OFFLINE"

	DefaultMaxParallelIOJobs = 1
	DefaultLogLimitBytes     = 10 << 20
)

type DesiredStateError struct {
	Code      string
	FieldPath string
}

func (e *DesiredStateError) Error() string {
	return e.Code + ": " + e.FieldPath
}

type RuntimePolicy struct {
	MaxParallelIOJobs uint32 `json:"max_parallel_io_jobs"`
	LogLimitBytes     uint64 `json:"log_limit_bytes"`
}

type DesiredState struct {
	AgentID       uuid.UUID
	Revision      int64
	GeneratedAt   time.Time
	ConfigHash    string
	RuntimePolicy RuntimePolicy
}

func NewDefaultDesiredState(agentID uuid.UUID, revision int64, generatedAt time.Time) (DesiredState, error) {
	state := DesiredState{
		AgentID:     agentID,
		Revision:    revision,
		GeneratedAt: generatedAt.UTC(),
		RuntimePolicy: RuntimePolicy{
			MaxParallelIOJobs: DefaultMaxParallelIOJobs,
			LogLimitBytes:     DefaultLogLimitBytes,
		},
	}
	canonical, err := state.CanonicalJSON()
	if err != nil {
		return DesiredState{}, err
	}
	hash := sha256.Sum256(canonical)
	state.ConfigHash = "sha256:" + hex.EncodeToString(hash[:])
	return state, nil
}

func (s DesiredState) CanonicalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Plans         []struct{}    `json:"plans"`
		Repositories  []struct{}    `json:"repositories"`
		RuntimePolicy RuntimePolicy `json:"runtime_policy"`
	}{
		Plans:         []struct{}{},
		Repositories:  []struct{}{},
		RuntimePolicy: s.RuntimePolicy,
	})
}

func (s DesiredState) Validate() error {
	switch {
	case s.AgentID == uuid.Nil:
		return &DesiredStateError{Code: "AGENT_ID_INVALID", FieldPath: "agent_id"}
	case s.Revision < 1:
		return &DesiredStateError{Code: "REVISION_INVALID", FieldPath: "revision"}
	case s.GeneratedAt.IsZero():
		return &DesiredStateError{Code: "GENERATED_AT_INVALID", FieldPath: "generated_at"}
	case s.RuntimePolicy.MaxParallelIOJobs < 1 || s.RuntimePolicy.MaxParallelIOJobs > 16:
		return &DesiredStateError{Code: "MAX_PARALLEL_IO_JOBS_INVALID", FieldPath: "runtime_policy.max_parallel_io_jobs"}
	case s.RuntimePolicy.LogLimitBytes < 64<<10 || s.RuntimePolicy.LogLimitBytes > DefaultLogLimitBytes:
		return &DesiredStateError{Code: "LOG_LIMIT_BYTES_INVALID", FieldPath: "runtime_policy.log_limit_bytes"}
	}
	canonical, err := s.CanonicalJSON()
	if err != nil {
		return err
	}
	hash := sha256.Sum256(canonical)
	expected := "sha256:" + hex.EncodeToString(hash[:])
	if s.ConfigHash != expected {
		return &DesiredStateError{Code: "CONFIG_HASH_MISMATCH", FieldPath: "config_hash"}
	}
	return nil
}

type AgentHealthCheck struct {
	Name      string
	Healthy   bool
	ErrorCode string
}

type AgentHeartbeat struct {
	AgentID          uuid.UUID
	BootID           string
	UptimeSeconds    int64
	AcceptedRevision int64
	ResticVersion    string
	StateFreeBytes   int64
	ClockOffsetMS    int64
	LocalTime        time.Time
	ErrorCode        string
	HealthChecks     []AgentHealthCheck
	ReceivedAt       time.Time
}

type AgentInventory struct {
	ID             uuid.UUID
	AgentID        uuid.UUID
	CapturedAt     time.Time
	Kernel         string
	OSRelease      string
	CPUArch        string
	AgentVersion   string
	ResticVersion  string
	Containerized  bool
	AvailableBytes map[string]uint64
	ClockOffsetMS  int64
	Capabilities   []string
}

type AgentConfigResult struct {
	Revision   int64
	ConfigHash string
	ErrorCode  string
	FieldPath  string
}

func (a Agent) HealthAt(now time.Time, offlineAfter time.Duration) string {
	if a.Status == AgentRevoked {
		return AgentRevoked
	}
	if a.LastSeenAt == nil || now.Sub(a.LastSeenAt.UTC()) > offlineAfter {
		return AgentHealthOffline
	}
	if a.HeartbeatErrorCode != "" || a.ConfigErrorCode != "" {
		return AgentHealthDegraded
	}
	return AgentHealthOnline
}

func ConfigError(err error) *DesiredStateError {
	var configError *DesiredStateError
	if errors.As(err, &configError) {
		return configError
	}
	return &DesiredStateError{Code: "CONFIG_INVALID", FieldPath: ""}
}
