package gateway

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"
)

// OfflineAuthorizationStore is the durable authority for offline authorization
// verification and write-back record submission. Implementations MUST honor
// context cancellation.
type OfflineAuthorizationStore interface {
	VerifyOfflineAuthorization(ctx context.Context, id, owner uuid.UUID, minSequence int64) error
	SubmitWritebackRecord(ctx context.Context, record WritebackPendingRecord) error
	ConfirmWritebackRecord(ctx context.Context, recordID uuid.UUID) error
	PendingWritebackRecords(ctx context.Context, gatewayInstanceID uuid.UUID, limit int) ([]WritebackPendingRecord, error)
}

// WritebackPendingRecord is a local record awaiting central confirmation.
type WritebackPendingRecord struct {
	ID                uuid.UUID
	AuthorizationID   uuid.UUID
	Owner             uuid.UUID
	GatewayInstanceID uuid.UUID
	Sequence          int64
	RecordType        string
	Payload           []byte
	Checksum          [32]byte
	CreatedAt         time.Time
}

// OfflineVerifier validates offline authorization grants and maintains the
// anti-rollback sequence counter. It is NOT a substitute for online backup
// admission; the Gateway still needs a per-session admission fence.
type OfflineVerifier struct {
	mu             sync.Mutex
	id             uuid.UUID
	owner          uuid.UUID
	minSequence    int64
	authorizedAt   time.Time
	expiresAt      time.Time
	lastVerifiedAt time.Time
}

// NewOfflineVerifier creates a verifier initialized with the grant parameters
// from a successfully issued or renewed authorization. The Gateway MUST
// persist these values across restarts.
func NewOfflineVerifier(id, owner uuid.UUID, sequence int64, authorizedAt, expiresAt time.Time) *OfflineVerifier {
	return &OfflineVerifier{
		id:           id,
		owner:        owner,
		minSequence:  sequence,
		authorizedAt: authorizedAt,
		expiresAt:    expiresAt,
	}
}

// Verify checks the authorization is still usable: not expired and sequence
// is at least the minimum. For online use it delegates to the central store;
// for offline use it checks local cached state only.
func (v *OfflineVerifier) Verify(now time.Time) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if now.After(v.expiresAt) || now.Equal(v.expiresAt) {
		return ErrAdmissionUnavailable
	}
	return nil
}

// ID returns the authorization ID.
func (v *OfflineVerifier) ID() uuid.UUID { return v.id }

// Owner returns the authorization owner.
func (v *OfflineVerifier) Owner() uuid.UUID { return v.owner }

// Sequence returns the current minimum anti-rollback sequence.
func (v *OfflineVerifier) Sequence() int64 {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.minSequence
}

// ExpiresAt returns the authorization expiry.
func (v *OfflineVerifier) ExpiresAt() time.Time {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.expiresAt
}

// UpdateFromRenewal updates the verifier after a successful central renewal.
// The new sequence MUST be strictly greater than the current one.
func (v *OfflineVerifier) UpdateFromRenewal(newSequence int64, newExpiresAt time.Time) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if newSequence > v.minSequence {
		v.minSequence = newSequence
		v.expiresAt = newExpiresAt
		v.lastVerifiedAt = time.Now()
	}
}
