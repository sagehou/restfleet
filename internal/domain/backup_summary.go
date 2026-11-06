package domain

import (
	"errors"
	"math"
	"time"
)

type BackupSummary struct {
	FilesNew            int64      `json:"files_new"`
	FilesChanged        int64      `json:"files_changed"`
	FilesUnmodified     int64      `json:"files_unmodified"`
	DirsNew             int64      `json:"dirs_new"`
	DirsChanged         int64      `json:"dirs_changed"`
	DirsUnmodified      int64      `json:"dirs_unmodified"`
	DataAdded           int64      `json:"data_added"`
	DataAddedPacked     int64      `json:"data_added_packed"`
	TotalFilesProcessed int64      `json:"total_files_processed"`
	TotalBytesProcessed int64      `json:"total_bytes_processed"`
	BackupStart         *time.Time `json:"backup_start,omitempty"`
	BackupEnd           *time.Time `json:"backup_end,omitempty"`
	TotalDuration       float64    `json:"total_duration"`
	WarningCount        int64      `json:"warning_count"`
}

func (s BackupSummary) Validate() error {
	for _, n := range []int64{s.FilesNew, s.FilesChanged, s.FilesUnmodified, s.DirsNew, s.DirsChanged, s.DirsUnmodified, s.DataAdded, s.DataAddedPacked, s.TotalFilesProcessed, s.TotalBytesProcessed, s.WarningCount} {
		if n < 0 {
			return errors.New("invalid backup summary counter")
		}
	}
	if s.TotalDuration < 0 || math.IsNaN(s.TotalDuration) || math.IsInf(s.TotalDuration, 0) {
		return errors.New("invalid backup duration")
	}
	if s.BackupStart != nil && s.BackupEnd != nil && s.BackupEnd.Before(*s.BackupStart) {
		return errors.New("invalid backup time range")
	}
	return nil
}
