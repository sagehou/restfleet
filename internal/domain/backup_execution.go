package domain

import "github.com/google/uuid"

const BackupExecutionCapability = "backup_execution_v1"

func RetryableBackupError(code string) bool {
	return code == "REPOSITORY_LOCKED" || code == "GATEWAY_BUSY"
}

// Zero values retain the V1 defaults; configured values are bounded at both
// control-plane admission and Agent configuration activation.
type BackupRetryPolicy struct {
	InitialBackoffSeconds int `json:"initial_backoff_seconds,omitempty"`
	MaxAttempts           int `json:"max_attempts,omitempty"`
	MaxBackoffSeconds     int `json:"max_backoff_seconds,omitempty"`
}

func (p BackupRetryPolicy) Attempts() int {
	if p.MaxAttempts == 0 {
		return 1
	}
	return p.MaxAttempts
}

func (p BackupRetryPolicy) Backoff(attempt int) int {
	initial, maximum := p.InitialBackoffSeconds, p.MaxBackoffSeconds
	if initial == 0 {
		initial = 60
	}
	if maximum == 0 {
		maximum = 1800
	}
	for i := 1; i < attempt && initial < maximum; i++ {
		initial *= 2
	}
	if initial > maximum {
		return maximum
	}
	return initial
}

type BackupResourceLimits struct {
	LimitDownloadKiB   int `json:"limit_download_kib,omitempty"`
	LimitUploadKiB     int `json:"limit_upload_kib,omitempty"`
	MaxDurationSeconds int `json:"max_duration_seconds,omitempty"`
	ReadConcurrency    int `json:"read_concurrency,omitempty"`
}

type BackupExecution struct {
	RetryPolicy    *BackupRetryPolicy    `json:"retry_policy,omitempty"`
	ResourceLimits *BackupResourceLimits `json:"resource_limits,omitempty"`
	PreHookIDs     []uuid.UUID           `json:"pre_hook_ids,omitempty"`
	PostHookIDs    []uuid.UUID           `json:"post_hook_ids,omitempty"`
}

func (c TemplateConfig) BackupExecution() *BackupExecution {
	if c.RetryPolicy == nil && c.ResourceLimits == nil && len(c.PreHookIDs) == 0 && len(c.PostHookIDs) == 0 {
		return nil
	}
	return &BackupExecution{RetryPolicy: c.RetryPolicy, ResourceLimits: c.ResourceLimits, PreHookIDs: c.PreHookIDs, PostHookIDs: c.PostHookIDs}
}

func (c *BackupExecution) Validate() error {
	if c == nil {
		return nil
	}
	if p := c.RetryPolicy; p != nil {
		if p.MaxAttempts < 0 || p.MaxAttempts > 10 || p.InitialBackoffSeconds < 0 || p.InitialBackoffSeconds > 86400 ||
			p.MaxBackoffSeconds < 0 || p.MaxBackoffSeconds > 86400 {
			return &TemplateError{Field: "retry_policy", Code: "OUT_OF_RANGE"}
		}
		initial, maximum := p.InitialBackoffSeconds, p.MaxBackoffSeconds
		if initial == 0 {
			initial = 60
		}
		if maximum == 0 {
			maximum = 1800
		}
		if initial > maximum {
			return &TemplateError{Field: "retry_policy", Code: "INVALID_BACKOFF"}
		}
	}
	if p := c.ResourceLimits; p != nil {
		if p.LimitDownloadKiB < 0 || p.LimitDownloadKiB > 1<<30 || p.LimitUploadKiB < 0 || p.LimitUploadKiB > 1<<30 ||
			p.ReadConcurrency < 0 || p.ReadConcurrency > 64 || p.MaxDurationSeconds < 0 || p.MaxDurationSeconds > 86400 {
			return &TemplateError{Field: "resource_limits", Code: "OUT_OF_RANGE"}
		}
	}
	for _, ids := range [][]uuid.UUID{c.PreHookIDs, c.PostHookIDs} {
		if len(ids) > 16 || duplicateUUID(ids) {
			return &TemplateError{Field: "hook_ids", Code: "INVALID"}
		}
		for _, id := range ids {
			if id.Version() != 7 || id.Variant() != uuid.RFC4122 {
				return &TemplateError{Field: "hook_ids", Code: "INVALID"}
			}
		}
	}
	return nil
}
