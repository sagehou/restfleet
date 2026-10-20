package domain

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
)

var (
	ErrPlanNameTaken      = errors.New("plan name already exists for this host")
	ErrPlanBindingInvalid = errors.New("plan host, agent and repository do not match")
	ErrPlanTemplateStale  = errors.New("template revision does not exist")
)

// PlanStatus values follow the documented lifecycle. Only an explicit apply
// moves a plan to ACTIVE; template edits alone never do.
var planStatuses = map[string]struct{}{
	"DRAFT": {}, "PENDING_APPLY": {}, "ACTIVE": {}, "PAUSED": {}, "INVALID": {}, "ARCHIVED": {},
}

var planOverrideKeys = map[string]struct{}{
	"paths": {}, "exclude_patterns": {}, "exclude_caches": {}, "one_file_system": {},
	"schedule_cron": {}, "schedule_timezone": {}, "jitter_seconds": {}, "misfire_grace_seconds": {},
	"retry_policy": {}, "retention_policy_id": {}, "pre_hook_ids": {}, "post_hook_ids": {},
	"resource_limits": {}, "enabled_by_default": {},
}

// Plan binds one Host, Agent, Repository and template revision. EffectiveConfig
// is the server-produced snapshot the Agent applies; the Agent never merges.
type Plan struct {
	ID               uuid.UUID
	Name             string
	HostID           uuid.UUID
	AgentID          uuid.UUID
	RepositoryID     uuid.UUID
	TemplateID       uuid.UUID
	TemplateRevision int64
	Overrides        json.RawMessage
	EffectiveConfig  TemplateConfig
	ConfigHash       string
	Status           string
	DesiredRevision  int64
	AcceptedRevision int64
	BackupHealth     string
	ArchivedAt       *time.Time
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// NormalizeOverrides keeps only documented keys and rejects unknown ones so a
// typo cannot silently disappear from the stored plan.
func NormalizeOverrides(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		return json.RawMessage(`{}`), nil
	}
	var values map[string]json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, &TemplateError{Field: "overrides", Code: "INVALID"}
	}
	for key := range values {
		if _, ok := planOverrideKeys[key]; !ok {
			return nil, &TemplateError{Field: "overrides." + key, Code: "UNKNOWN"}
		}
	}
	canonical, err := json.Marshal(values)
	if err != nil {
		return nil, &TemplateError{Field: "overrides", Code: "INVALID"}
	}
	return canonical, nil
}

// MergePlanConfig applies a JSON-merge-patch subset of overrides to the
// template revision body, then validates the merged result.
func MergePlanConfig(base TemplateConfig, overrides json.RawMessage) (TemplateConfig, error) {
	normalized, err := NormalizeOverrides(overrides)
	if err != nil {
		return TemplateConfig{}, err
	}
	raw, err := base.CanonicalJSON()
	if err != nil {
		return TemplateConfig{}, err
	}
	var merged map[string]json.RawMessage
	if err := json.Unmarshal(raw, &merged); err != nil {
		return TemplateConfig{}, err
	}
	var patch map[string]json.RawMessage
	if err := json.Unmarshal(normalized, &patch); err != nil {
		return TemplateConfig{}, err
	}
	for key, value := range patch {
		merged[key] = value
	}
	encoded, err := json.Marshal(merged)
	if err != nil {
		return TemplateConfig{}, err
	}
	var config TemplateConfig
	if err := json.Unmarshal(encoded, &config); err != nil {
		return TemplateConfig{}, &TemplateError{Field: "overrides", Code: "INVALID"}
	}
	if err := config.Validate(); err != nil {
		return TemplateConfig{}, err
	}
	return config, nil
}

func ValidPlanStatus(status string) bool {
	_, ok := planStatuses[status]
	return ok
}

var ErrPlanStateInvalid = errors.New("plan state does not allow this transition")

// PlanApplyTarget is one explicit plan the operator asked to move to a new
// template revision. The expected revision makes the batch conflict-safe.
type PlanApplyTarget struct {
	PlanID           uuid.UUID
	ExpectedRevision int64
}

// PlanApplyResult reports one plan independently so a batch can partially
// succeed without hiding which plans conflicted.
type PlanApplyResult struct {
	PlanID uuid.UUID
	Status string
	Code   string
}
