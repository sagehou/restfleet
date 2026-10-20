package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

var (
	ErrTemplateNameTaken = errors.New("template name already exists")
	ErrTemplateArchived  = errors.New("template is archived")
	ErrTemplateInUse     = errors.New("template is used by active plans")
)

// TemplateError names the first invalid field of a revision body.
type TemplateError struct {
	Field string
	Code  string
}

func (e *TemplateError) Error() string { return e.Field + ": " + e.Code }

// TemplateConfig is one immutable revision body. RetryPolicy and ResourceLimits
// stay opaque so the server stores exactly what a later scheduler understands;
// every other field is validated before the revision is written.
type TemplateConfig struct {
	Paths               []string       `json:"paths"`
	ExcludePatterns     []string       `json:"exclude_patterns"`
	ExcludeCaches       bool           `json:"exclude_caches"`
	OneFileSystem       bool           `json:"one_file_system"`
	ScheduleCron        string         `json:"schedule_cron"`
	ScheduleTimezone    string         `json:"schedule_timezone"`
	JitterSeconds       int            `json:"jitter_seconds"`
	MisfireGraceSeconds int            `json:"misfire_grace_seconds"`
	RetryPolicy         map[string]any `json:"retry_policy"`
	RetentionPolicyID   *uuid.UUID     `json:"retention_policy_id"`
	PreHookIDs          []uuid.UUID    `json:"pre_hook_ids"`
	PostHookIDs         []uuid.UUID    `json:"post_hook_ids"`
	ResourceLimits      map[string]any `json:"resource_limits"`
	EnabledByDefault    bool           `json:"enabled_by_default"`
}

// CanonicalJSON returns the deterministic encoding used for config_hash.
func (c TemplateConfig) CanonicalJSON() ([]byte, error) {
	return json.Marshal(c)
}

// ConfigHash returns the canonical SHA-256 of one revision body.
func (c TemplateConfig) ConfigHash() (string, error) {
	canonical, err := c.CanonicalJSON()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}

// Template is the mutable pointer; its body lives in an immutable revision.
type Template struct {
	ID              uuid.UUID
	Name            string
	Description     string
	CurrentRevision int64
	Config          TemplateConfig
	ArchivedAt      *time.Time
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

func (t Template) Archived() bool { return t.ArchivedAt != nil }

// Validate checks one revision body before persistence. It rejects fields the
// Agent could not schedule deterministically instead of accepting them softly.
func (c TemplateConfig) Validate() error {
	if len(c.Paths) == 0 {
		return &TemplateError{Field: "paths", Code: "REQUIRED"}
	}
	for i, path := range c.Paths {
		if strings.TrimSpace(path) == "" || strings.ContainsRune(path, 0) {
			return &TemplateError{Field: fmt.Sprintf("paths.%d", i), Code: "INVALID"}
		}
	}
	for i, pattern := range c.ExcludePatterns {
		if strings.ContainsRune(pattern, 0) {
			return &TemplateError{Field: fmt.Sprintf("exclude_patterns.%d", i), Code: "INVALID"}
		}
	}
	if err := validateCron(c.ScheduleCron); err != nil {
		return &TemplateError{Field: "schedule_cron", Code: "INVALID"}
	}
	if err := validateTimezone(c.ScheduleTimezone); err != nil {
		return &TemplateError{Field: "schedule_timezone", Code: "INVALID"}
	}
	if c.JitterSeconds < 0 || c.JitterSeconds > 3600 {
		return &TemplateError{Field: "jitter_seconds", Code: "OUT_OF_RANGE"}
	}
	if c.MisfireGraceSeconds < 0 || c.MisfireGraceSeconds > 86400 {
		return &TemplateError{Field: "misfire_grace_seconds", Code: "OUT_OF_RANGE"}
	}
	if duplicateUUID(c.PreHookIDs) || duplicateUUID(c.PostHookIDs) {
		return &TemplateError{Field: "hook_ids", Code: "DUPLICATE"}
	}
	if c.RetentionPolicyID != nil && *c.RetentionPolicyID == uuid.Nil {
		return &TemplateError{Field: "retention_policy_id", Code: "INVALID"}
	}
	return nil
}

func duplicateUUID(values []uuid.UUID) bool {
	seen := make(map[uuid.UUID]struct{}, len(values))
	for _, value := range values {
		if _, ok := seen[value]; ok {
			return true
		}
		seen[value] = struct{}{}
	}
	return false
}

func validateTimezone(name string) error {
	if name == "" || name == "Local" {
		return errors.New("timezone is required")
	}
	if _, err := time.LoadLocation(name); err != nil {
		return err
	}
	return nil
}

var cronFields = [5]struct{ min, max int }{
	{0, 59}, {0, 23}, {1, 31}, {1, 12}, {0, 6},
}

// validateCron accepts the standard 5-field form. It checks syntax and range
// only; next-occurrence semantics belong to the Agent scheduler.
func validateCron(expr string) error {
	fields := strings.Fields(expr)
	if len(fields) != len(cronFields) {
		return errors.New("cron must have five fields")
	}
	for i, field := range fields {
		if err := validateCronField(field, cronFields[i].min, cronFields[i].max); err != nil {
			return err
		}
	}
	return nil
}

func validateCronField(field string, min, max int) error {
	for _, part := range strings.Split(field, ",") {
		if part == "" {
			return errors.New("empty list item")
		}
		step := 1
		if base, rawStep, ok := strings.Cut(part, "/"); ok {
			parsed, err := strconv.Atoi(rawStep)
			if err != nil || parsed < 1 {
				return errors.New("invalid step")
			}
			step = parsed
			part = base
		}
		if part == "*" {
			continue
		}
		if low, high, ok := strings.Cut(part, "-"); ok {
			first, err := strconv.Atoi(low)
			if err != nil {
				return err
			}
			last, err := strconv.Atoi(high)
			if err != nil {
				return err
			}
			if first < min || last > max || first > last {
				return errors.New("range out of bounds")
			}
			if step > last-first+1 {
				return errors.New("step exceeds range")
			}
			continue
		}
		value, err := strconv.Atoi(part)
		if err != nil || value < min || value > max {
			return errors.New("value out of bounds")
		}
	}
	return nil
}
