package domain

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

var (
	ErrWritebackUnavailable = errors.New("write-back area unavailable")
	ErrWritebackFull        = errors.New("write-back area capacity exceeded")
	ErrWritebackCorruption  = errors.New("write-back record integrity failure")
)

// WritebackRecord is a durable pending record in the Gateway's protected
// persistent write-back area. Each record carries trusted owner/instance/fence
// binding, a unique identifier and an ordered version for replay detection.
//
// The central server MUST reject conflict replay, skip-sequence, tampering,
// cross-binding and non-token-only configuration changes. DB commit precedes
// local confirmation; confirmation loss triggers idempotent replay.
type WritebackRecord struct {
	ID                  uuid.UUID
	AuthorizationID     uuid.UUID
	Owner               uuid.UUID
	GatewayInstanceID   uuid.UUID
	Sequence            int64
	RecordType          string // "AUDIT_EVENT", "TOKEN_REFRESH", "ADMISSION_CHANGE"
	Payload             []byte // Encrypted, source-authenticated
	Checksum            []byte // SHA-256 of plaintext before encryption
	CreatedAt           time.Time
	ConfirmedAt         *time.Time
}

// Pending reports whether this record has been confirmed by the central server.
func (r WritebackRecord) Pending() bool {
	return r.ConfirmedAt == nil
}

// WritebackAreaConfig defines the hard limits for the Gateway's durable
// write-back area on the protected persistent volume.
type WritebackAreaConfig struct {
	MaxBytes   int64 // Hard byte limit; MUST NOT be exceeded
	MaxRecords int   // Hard record count limit
}

// DefaultWritebackAreaConfig returns conservative defaults per §7.10 step 2.
func DefaultWritebackAreaConfig() WritebackAreaConfig {
	return WritebackAreaConfig{
		MaxBytes:   16 * 1024 * 1024, // 16 MiB
		MaxRecords: 4096,
	}
}

// WritebackSubmission is sent from the Gateway to the central server for
// durable persistence and confirmation.
type WritebackSubmission struct {
	RecordID          uuid.UUID
	AuthorizationID   uuid.UUID
	Owner             uuid.UUID
	GatewayInstanceID uuid.UUID
	Sequence          int64
	RecordType        string
	Payload           []byte // Encrypted
	Checksum          []byte
	CreatedAt         time.Time
}

// WritebackConfirmation is the central server's acknowledgment that a
// write-back record has been durably persisted.
type WritebackConfirmation struct {
	RecordID      uuid.UUID
	AuthorizationID uuid.UUID
	Sequence      int64
	ConfirmedAt   time.Time
}
