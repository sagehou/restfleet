package server

import (
	"bytes"
	"context"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/sagehou/restfleet/internal/domain"
	"github.com/sagehou/restfleet/internal/rclone"
)

// BackupMaterialStore is a central-only extension. It is not an Agent API.
type BackupMaterialStore interface {
	BackupAdmissionMaterial(context.Context, uuid.UUID, uuid.UUID, string) (domain.BackupMaterial, error)
	RefreshBackupAdmission(context.Context, uuid.UUID, uuid.UUID, string, int64, domain.SecretEnvelope) (domain.StorageCredential, error)
}

// WithBackupMaterial lends only the admitted cloud config to a trusted central
// runner. run MUST honor ctx, join its work and never retain plaintext. It MUST
// use the admission-aware supervisor; this method alone does not publish routes,
// audit data-plane requests, release the fence or authorize offline use.
func (c *ControlPlane) WithBackupMaterial(ctx context.Context, id, owner uuid.UUID,
	run func(context.Context, domain.BackupAdmission, []byte, string, func(context.Context, []byte) error) error,
) error {
	store, ok := c.store.(BackupMaterialStore)
	if !ok || len(c.masterKey) != 32 || c.gatewayPublicURL == "" || run == nil {
		return domain.ErrBackupAdmission
	}
	hash := c.credentialConfigurationHash()
	material, err := store.BackupAdmissionMaterial(ctx, id, owner, hash)
	if err != nil {
		return domain.ErrBackupAdmission
	}
	credential := material.Credential
	raw, err := openStorageSecret(c.masterKey, credential, material.Envelope)
	if err != nil {
		return domain.ErrBackupAdmission
	}
	defer clear(raw)
	previous, err := rclone.ParseConfig(string(raw), credential.RemoteName)
	if err != nil || storageProvider(previous) != credential.Provider {
		return domain.ErrBackupAdmission
	}
	workCtx, cancel := context.WithDeadline(ctx, material.Admission.ExpiresAt)
	var mu sync.Mutex
	closed := false
	defer func() {
		cancel()
		mu.Lock()
		closed = true
		mu.Unlock()
	}()
	persist := func(refreshCtx context.Context, nextRaw []byte) error {
		mu.Lock()
		defer mu.Unlock()
		if closed || workCtx.Err() != nil {
			return rclone.ErrRefreshPersist
		}
		next, err := rclone.ParseConfig(string(nextRaw), credential.RemoteName)
		if err != nil || !previous.SameExceptToken(next) {
			return rclone.ErrConfigChanged
		}
		before, after := previous.Bytes(), next.Bytes()
		defer clear(before)
		defer clear(after)
		if bytes.Equal(before, after) {
			return nil
		}
		nextCredential := credential
		nextCredential.SecretRevision++
		nextCredential.UpdatedAt = c.clock().UTC()
		sealed, err := c.sealStorageConfig(nextCredential, next)
		if err != nil {
			return rclone.ErrRefreshPersist
		}
		refreshCtx, stop := context.WithTimeout(refreshCtx, 3*time.Second)
		defer stop()
		stopParent := context.AfterFunc(workCtx, stop)
		defer stopParent()
		saved, err := store.RefreshBackupAdmission(refreshCtx, id, owner, hash, credential.SecretRevision, sealed)
		if err != nil {
			return rclone.ErrRefreshPersist
		}
		credential, previous = saved, next
		return nil
	}
	if workCtx.Err() != nil {
		return domain.ErrBackupAdmission
	}
	if err = run(workCtx, material.Admission, raw, credential.RemoteName, persist); err != nil || workCtx.Err() != nil {
		return domain.ErrBackupAdmission
	}
	return nil
}
