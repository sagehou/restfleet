package domain

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

const StorageProvider = "RCLONE_ONEDRIVE"

var (
	ErrStorageUnavailable   = errors.New("storage credential service is not configured")
	ErrCredentialDisabled   = errors.New("storage credential is disabled")
	ErrStorageTargetChanged = errors.New("storage target or crypt settings cannot be changed")
)

// StorageCredential contains metadata only; immutable encrypted versions live in secrets.
type StorageCredential struct {
	ID             uuid.UUID
	Name           string
	Provider       string
	RemoteName     string
	Status         string
	SecretRef      uuid.UUID
	SecretRevision int64
	Revision       int64
	CreatedAt      time.Time
	UpdatedAt      time.Time
}
