package domain

import (
	"errors"
	"time"
)

const BackupLogsCapability = "backup_logs_v1"
const MaxBackupLogChunkBytes = 32 << 10

// BackupLogEntry is a projection of engine activity. Raw stdout, stderr,
// filenames, commands and unrecognised strings never enter this model.
type BackupLogEntry struct {
	Sequence   uint64    `json:"sequence"`
	ObservedAt time.Time `json:"observed_at"`
	Section    string    `json:"section"`
	Level      string    `json:"level"`
	Code       string    `json:"code"`
	FilesDone  int64     `json:"files_done"`
	BytesDone  int64     `json:"bytes_done"`
}

func ValidBackupErrorCode(code string) bool {
	switch code {
	case "", "EXECUTOR_UNAVAILABLE", "IDENTITY_UNAVAILABLE", "CREDENTIAL_UNAVAILABLE", "WORKDIR_UNAVAILABLE", "CONFIG_INVALID",
		"CANCELED", "GATEWAY_SESSION_FAILED", "GATEWAY_CLEANUP_UNCONFIRMED", "GATEWAY_BUSY", "INVALID_ENGINE_OUTPUT", "PARTIAL_READ",
		"REPOSITORY_LOCKED", "WRONG_REPOSITORY_PASSWORD", "REPOSITORY_MISSING", "BACKUP_FAILED", "UNKNOWN_EXIT_CODE",
		"STATE_STORAGE_UNAVAILABLE", "BACKUP_TIMED_OUT", "PRE_HOOK_FAILED", "POST_HOOK_FAILED", "CONFIG_SUPERSEDED",
		"LOCAL_OPERATION_RECOVERED", "MISFIRE_GRACE_EXCEEDED", "CONCURRENCY_SKIPPED", "AGENT_REJECTED":
		return true
	}
	return false
}

func (e BackupLogEntry) Validate() error {
	if e.Sequence == 0 || e.Sequence > 1<<62 || e.ObservedAt.IsZero() || e.FilesDone < 0 || e.BytesDone < 0 {
		return errors.New("invalid backup log metadata")
	}
	if e.Section != "backup" && e.Section != "pre_hook" && e.Section != "post_hook" && e.Section != "agent" {
		return errors.New("invalid backup log section")
	}
	if e.Level != "INFO" && e.Level != "WARN" && e.Level != "ERROR" {
		return errors.New("invalid backup log level")
	}
	switch e.Code {
	case "BACKUP_STARTED", "BACKUP_PROGRESS", "BACKUP_SUMMARY", "BACKUP_SUCCEEDED", "BACKUP_SUCCEEDED_WITH_WARNINGS", "HOOK_STARTED", "HOOK_FINISHED", "ENGINE_READ_ERROR", "LOG_TRUNCATED":
		return nil
	}
	if e.Code != "" && ValidBackupErrorCode(e.Code) {
		return nil
	}
	return errors.New("invalid backup log code")
}
