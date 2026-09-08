package domain

import (
	"encoding/hex"
	"errors"
	"time"

	"github.com/google/uuid"
)

var ErrBackupAdmission = errors.New("backup admission is unavailable or does not match")

// BackupAdmissionRequest is central-only. ID and Owner are durable UUIDv7
// identities allocated by the trusted caller, not bearer capabilities. AgentID
// MUST come from authenticated identity; no caller selects a Host or Repository.
type BackupAdmissionRequest struct {
	ID, Owner, AgentID, DeliveryID uuid.UUID
	ConfigurationHash              string
	Lifetime                       time.Duration
}

func (r BackupAdmissionRequest) Validate() error {
	for _, id := range []uuid.UUID{r.ID, r.Owner, r.AgentID, r.DeliveryID} {
		if id.Version() != 7 || id.Variant() != uuid.RFC4122 {
			return ErrBackupAdmission
		}
	}
	hash, err := hex.DecodeString(r.ConfigurationHash)
	if err != nil || len(hash) != 32 || hex.EncodeToString(hash) != r.ConfigurationHash ||
		r.Lifetime < time.Minute || r.Lifetime > 24*time.Hour || r.Lifetime%time.Second != 0 {
		return ErrBackupAdmission
	}
	return nil
}

// BackupAdmission is an exclusive data-plane reservation, NOT a backup result,
// job, Agent ACK, session password or proof that a backend is READY. Expiration
// revokes use but does not prove cleanup: unreleased reservations still fence
// central writers. Each actual backup needs its own scoped Gateway session.
type BackupAdmission struct {
	ID, Owner, AgentID, HostID, RepositoryID, GatewayID, StorageCredentialID uuid.UUID
	DeliveryID, GatewaySecretRef, ResticSecretRef                            uuid.UUID
	ConfigurationHash                                                        string
	CreatedAt, ExpiresAt                                                     time.Time
	ReleasedAt                                                               *time.Time
}
