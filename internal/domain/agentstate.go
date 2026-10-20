package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
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

// PlanSchedule is the Agent-visible scheduling subset of one plan. It carries
// no secrets and is part of the canonical config hash.
type PlanSchedule struct {
	PlanID              uuid.UUID `json:"plan_id"`
	Revision            int64     `json:"revision"`
	Name                string    `json:"name"`
	HostID              uuid.UUID `json:"host_id"`
	RepositoryID        uuid.UUID `json:"repository_id"`
	Cron                string    `json:"cron"`
	Timezone            string    `json:"timezone"`
	JitterSeconds       int       `json:"jitter_seconds"`
	MisfireGraceSeconds int       `json:"misfire_grace_seconds"`
	ConcurrencyPolicy   string    `json:"concurrency_policy"`
	Paths               []string  `json:"paths"`
	ExcludePatterns     []string  `json:"exclude_patterns"`
	ExcludeCaches       bool      `json:"exclude_caches"`
	OneFileSystem       bool      `json:"one_file_system"`
	ConfigHash          string    `json:"config_hash"`
}

type DesiredState struct {
	AgentID       uuid.UUID
	Revision      int64
	GeneratedAt   time.Time
	ConfigHash    string
	RuntimePolicy RuntimePolicy
	Plans         []PlanSchedule
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
	plans := append([]PlanSchedule{}, s.Plans...)
	sort.Slice(plans, func(i, j int) bool {
		if plans[i].PlanID != plans[j].PlanID {
			return plans[i].PlanID.String() < plans[j].PlanID.String()
		}
		return plans[i].Revision < plans[j].Revision
	})
	return json.Marshal(struct {
		Plans         []PlanSchedule `json:"plans"`
		Repositories  []struct{}     `json:"repositories"`
		RuntimePolicy RuntimePolicy  `json:"runtime_policy"`
	}{
		Plans:         plans,
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
	for _, plan := range s.Plans {
		if err := plan.Validate(); err != nil {
			return err
		}
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

// Validate rejects a plan the Agent could not schedule deterministically.
func (p PlanSchedule) Validate() error {
	switch {
	case p.PlanID.Version() != 7 || p.PlanID.Variant() != uuid.RFC4122:
		return &DesiredStateError{Code: "PLAN_ID_INVALID", FieldPath: "plans.plan_id"}
	case p.Revision < 1:
		return &DesiredStateError{Code: "PLAN_REVISION_INVALID", FieldPath: "plans.revision"}
	case p.Name == "" || len(p.Name) > 128:
		return &DesiredStateError{Code: "PLAN_NAME_INVALID", FieldPath: "plans.name"}
	case p.RepositoryID.Version() != 7 || p.RepositoryID.Variant() != uuid.RFC4122:
		return &DesiredStateError{Code: "PLAN_REPOSITORY_INVALID", FieldPath: "plans.repository_id"}
	case len(p.Paths) == 0:
		return &DesiredStateError{Code: "PLAN_PATHS_REQUIRED", FieldPath: "plans.paths"}
	case p.JitterSeconds < 0 || p.JitterSeconds > 3600:
		return &DesiredStateError{Code: "PLAN_JITTER_INVALID", FieldPath: "plans.jitter_seconds"}
	case p.MisfireGraceSeconds < 0 || p.MisfireGraceSeconds > 86400:
		return &DesiredStateError{Code: "PLAN_MISFIRE_INVALID", FieldPath: "plans.misfire_grace_seconds"}
	case p.ConcurrencyPolicy != "" && p.ConcurrencyPolicy != "FORBID" && p.ConcurrencyPolicy != "ALLOW":
		return &DesiredStateError{Code: "PLAN_CONCURRENCY_INVALID", FieldPath: "plans.concurrency_policy"}
	}
	if validateCron(p.Cron) != nil {
		return &DesiredStateError{Code: "PLAN_CRON_INVALID", FieldPath: "plans.cron"}
	}
	if validateTimezone(p.Timezone) != nil {
		return &DesiredStateError{Code: "PLAN_TIMEZONE_INVALID", FieldPath: "plans.timezone"}
	}
	return nil
}
