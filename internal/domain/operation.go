package domain

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

var (
	ErrOperationTransition = errors.New("invalid operation transition")
	ErrCredentialTestBusy  = errors.New("credential test already active")
	ErrJobLeaseLost        = errors.New("job lease is no longer owned")
	ErrIdempotencyReused   = errors.New("idempotency key reused")
)

// Operation is one immutable execution identity; retry never reopens a terminal operation.
type Operation struct {
	ID                                                  uuid.UUID
	Type, Status, Source                                string
	StorageCredentialID                                 uuid.UUID
	SecretRevision                                      int64
	RequestedByUserID                                   uuid.UUID
	Attempt                                             int
	CreatedAt                                           time.Time
	DispatchedAt, AcknowledgedAt, StartedAt, FinishedAt *time.Time
	ErrorCode                                           string
}

func OperationTerminal(status string) bool {
	switch status {
	case "SUCCEEDED", "SUCCEEDED_WITH_WARNINGS", "FAILED", "CANCELED", "TIMED_OUT", "LOST", "REJECTED":
		return true
	}
	return false
}

func ValidateOperationTransition(from, to string) error {
	switch from {
	case "QUEUED":
		if to == "DISPATCHED" || to == "REJECTED" || to == "CANCELED" {
			return nil
		}
	case "DISPATCHED":
		if to == "ACKNOWLEDGED" || to == "LOST" || to == "FAILED" {
			return nil
		}
	case "ACKNOWLEDGED":
		if to == "RUNNING" {
			return nil
		}
	case "RUNNING":
		if to == "SUCCEEDED" || to == "SUCCEEDED_WITH_WARNINGS" || to == "FAILED" || to == "CANCEL_REQUESTED" || to == "TIMED_OUT" {
			return nil
		}
	case "CANCEL_REQUESTED":
		if to == "CANCELED" {
			return nil
		}
	}
	return ErrOperationTransition
}

// CredentialJob contains only lease and metadata, never materialized secrets.
type CredentialJob struct {
	ID, Owner      uuid.UUID
	Operation      Operation
	Credential     StorageCredential
	LeaseExpiresAt time.Time
}

// CredentialTestOutcome codes are a closed vocabulary; subprocess/database
// errors are never persisted verbatim as operation or credential metadata.
func ValidCredentialTestCode(code string) bool {
	switch code {
	case "", "CONNECTION_FAILED", "TEST_TIMED_OUT", "CONFIG_UNSAFE", "REFRESH_FAILED",
		"CREDENTIAL_CHANGED", "CREDENTIAL_DISABLED", "SECRET_UNAVAILABLE", "WORKER_LOST":
		return true
	}
	return false
}
