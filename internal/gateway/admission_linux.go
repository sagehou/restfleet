package gateway

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/sagehou/restfleet/internal/domain"
)

var ErrAdmissionUnavailable = errors.New("gateway backup admission unavailable; fence retained")

// AdmissionStore is the central durable authority, not an Agent-supplied port.
// Implementations MUST honor context cancellation and commit release atomically.
type AdmissionStore interface {
	CheckBackupAdmission(context.Context, uuid.UUID, uuid.UUID, string) (domain.BackupAdmission, error)
	ReleaseBackupAdmission(context.Context, uuid.UUID, uuid.UUID) error
}

type admittedBackup struct {
	store     AdmissionStore
	id, owner uuid.UUID
	hash      string
}

// WithAdmittedBackup is an ONLINE central integration seam, not a public API or
// the offline authorization policy. The caller MUST persist id/owner, audit
// secret access and supply matching central material. Use ONE supervisor/runtime
// owner; this does not provide cross-process takeover or prove crash cleanup.
// Only a wholly successful run, refresh persistence and end audit auto-release.
// Every ambiguous outcome retains the durable fence for trusted recovery.
func (s *Supervisor) WithAdmittedBackup(ctx context.Context, store AdmissionStore, id, owner uuid.UUID, configurationHash string,
	request BackupRequest, persist func(context.Context, []byte) error, run func(context.Context, Access) error,
) error {
	if store == nil || id.Version() != 7 || id.Variant() != uuid.RFC4122 ||
		owner.Version() != 7 || owner.Variant() != uuid.RFC4122 || len(configurationHash) != 64 {
		return ErrAdmissionUnavailable
	}
	for _, c := range configurationHash {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return ErrAdmissionUnavailable
		}
	}
	return s.withBackup(ctx, request, persist, run, &admittedBackup{store: store, id: id, owner: owner, hash: configurationHash})
}

func (a *admittedBackup) check(ctx context.Context, request BackupRequest) (domain.BackupAdmission, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	grant, err := a.store.CheckBackupAdmission(ctx, a.id, a.owner, a.hash)
	if err != nil || ctx.Err() != nil || grant.ID != a.id || grant.Owner != a.owner || grant.ConfigurationHash != a.hash ||
		grant.HostID != request.Binding.HostID || grant.RepositoryID != request.Binding.RepositoryID ||
		grant.GatewayID != request.Binding.GatewayID || grant.StorageCredentialID != request.CredentialID ||
		grant.ReleasedAt != nil || !grant.ExpiresAt.After(time.Now()) {
		return domain.BackupAdmission{}, ErrAdmissionUnavailable
	}
	return grant, nil
}

func (a *admittedBackup) start(parent context.Context, request BackupRequest) (context.Context, func(error) error, error) {
	grant, err := a.check(parent, request)
	if err != nil {
		return parent, nil, err
	}
	ctx, cancel := context.WithDeadline(parent, grant.ExpiresAt)
	watchCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-watchCtx.Done():
				return
			case <-ticker.C:
				next, err := a.check(watchCtx, request)
				// Stopping the watcher during cleanup is not an authority failure.
				if watchCtx.Err() != nil {
					return
				}
				if err != nil || !next.ExpiresAt.Equal(grant.ExpiresAt) {
					cancel()
					return
				}
			}
		}
	}()
	finish := func(result error) error {
		stop()
		<-done
		defer cancel()
		if result != nil {
			return result
		}
		if ctx.Err() != nil {
			return ErrAdmissionUnavailable
		}
		releaseCtx, releaseCancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		defer releaseCancel()
		if a.store.ReleaseBackupAdmission(releaseCtx, a.id, a.owner) != nil || releaseCtx.Err() != nil {
			return ErrAdmissionUnavailable
		}
		return nil
	}
	return ctx, finish, nil
}
