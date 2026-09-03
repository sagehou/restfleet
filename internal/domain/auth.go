package domain

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

const (
	RoleAdmin  = "ADMIN"
	RoleViewer = "VIEWER"

	UserActive   = "ACTIVE"
	UserDisabled = "DISABLED"
)

var (
	ErrNotFound        = errors.New("not found")
	ErrBootstrapClosed = errors.New("bootstrap is closed")
	ErrSessionExpired  = errors.New("session expired")
)

// User is the transport- and persistence-independent control-plane identity.
type User struct {
	ID           uuid.UUID
	Username     string
	DisplayName  string
	PasswordHash string
	Role         string
	Status       string
	LastLoginAt  *time.Time
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// Session stores only hashes of browser bearer and CSRF tokens.
type Session struct {
	ID                uuid.UUID
	UserID            uuid.UUID
	TokenHash         []byte
	CSRFSecretHash    []byte
	CreatedAt         time.Time
	LastSeenAt        time.Time
	IdleExpiresAt     time.Time
	AbsoluteExpiresAt time.Time
	RevokedAt         *time.Time
	IPHash            []byte
	UserAgentSummary  string
}

// AuthenticatedSession combines the authenticated user with session metadata.
type AuthenticatedSession struct {
	User    User
	Session Session
}
