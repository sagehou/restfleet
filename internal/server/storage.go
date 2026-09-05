package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/sagehou/restfleet/internal/domain"
	"github.com/sagehou/restfleet/internal/rclone"
	"github.com/sagehou/restfleet/internal/security"
)

var ErrForbidden = errors.New("administrator access required")

func (c *ControlPlane) requireStorageAdmin(ctx context.Context, actor domain.User, meta RequestMeta) error {
	if actor.Role != domain.RoleAdmin {
		if err := c.RecordDenied(ctx, "STORAGE_CREDENTIAL_CHANGE", "STORAGE_CREDENTIAL", "ROLE_DENIED", meta); err != nil {
			return err
		}
		return ErrForbidden
	}
	if len(c.masterKey) != 32 {
		return domain.ErrStorageUnavailable
	}
	return nil
}

func (c *ControlPlane) StorageCredentials(ctx context.Context, after uuid.UUID, limit int) ([]domain.StorageCredential, error) {
	if limit < 1 || limit > 201 {
		return nil, &ValidationError{Field: "limit", Code: "INVALID_LIMIT"}
	}
	return c.store.StorageCredentials(ctx, after, limit)
}

func (c *ControlPlane) StorageCredential(ctx context.Context, id uuid.UUID) (domain.StorageCredential, error) {
	return c.store.StorageCredential(ctx, id)
}

func (c *ControlPlane) CreateStorageCredential(ctx context.Context, name, remote, raw string, actor domain.User, meta RequestMeta) (domain.StorageCredential, error) {
	if err := c.requireStorageAdmin(ctx, actor, meta); err != nil {
		return domain.StorageCredential{}, err
	}
	name = strings.TrimSpace(name)
	if !utf8.ValidString(name) || utf8.RuneCountInString(name) < 1 || utf8.RuneCountInString(name) > 128 || strings.IndexFunc(name, unicode.IsControl) >= 0 {
		return domain.StorageCredential{}, &ValidationError{Field: "name", Code: "INVALID_NAME"}
	}
	config, err := rclone.ParseConfig(raw, remote)
	if err != nil {
		return domain.StorageCredential{}, c.invalidStorageConfig(ctx, meta)
	}
	id, err := uuid.NewV7()
	if err != nil {
		return domain.StorageCredential{}, err
	}
	now := c.clock().UTC()
	credential := domain.StorageCredential{
		ID: id, Name: name, Provider: domain.StorageProvider, RemoteName: remote,
		Status: "UNTESTED", SecretRevision: 1, Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	return c.saveStorageConfig(ctx, credential, 0, config, actor, meta, "STORAGE_CREDENTIAL_CREATE")
}

func (c *ControlPlane) ReplaceStorageCredential(ctx context.Context, id uuid.UUID, revision int64, raw string, actor domain.User, meta RequestMeta) (domain.StorageCredential, error) {
	if err := c.requireStorageAdmin(ctx, actor, meta); err != nil {
		return domain.StorageCredential{}, err
	}
	credential, err := c.store.StorageCredential(ctx, id)
	if err != nil {
		return domain.StorageCredential{}, err
	}
	if revision != credential.Revision {
		return domain.StorageCredential{}, domain.ErrRevisionConflict
	}
	if credential.Status == "DISABLED" {
		return domain.StorageCredential{}, domain.ErrCredentialDisabled
	}
	next, err := rclone.ParseConfig(raw, credential.RemoteName)
	if err != nil {
		return domain.StorageCredential{}, c.invalidStorageConfig(ctx, meta)
	}
	audit, err := c.userAudit("STORAGE_SECRET_ACCESS", "STORAGE_CREDENTIAL", id, actor.ID, meta, "VALIDATE_REPLACEMENT")
	if err != nil {
		return domain.StorageCredential{}, err
	}
	// Fail closed before accessing plaintext when the audit store is unavailable.
	if err := c.store.RecordAudit(ctx, audit); err != nil {
		return domain.StorageCredential{}, err
	}
	envelope, err := c.store.StorageCredentialSecret(ctx, credential.SecretRef)
	if err != nil {
		return domain.StorageCredential{}, err
	}
	oldBytes, err := openStorageSecret(c.masterKey, credential, envelope)
	if err != nil {
		return domain.StorageCredential{}, err
	}
	defer clear(oldBytes)
	previous, err := rclone.ParseConfig(string(oldBytes), credential.RemoteName)
	if err != nil {
		return domain.StorageCredential{}, domain.ErrStorageUnavailable
	}
	if !previous.SameTarget(next) {
		if err := c.RecordDenied(ctx, "STORAGE_CREDENTIAL_REPLACE", "STORAGE_CREDENTIAL", "STORAGE_TARGET_CHANGED", meta); err != nil {
			return domain.StorageCredential{}, err
		}
		return domain.StorageCredential{}, domain.ErrStorageTargetChanged
	}
	credential.SecretRevision++
	credential.Status = "UNTESTED"
	credential.UpdatedAt = c.clock().UTC()
	return c.saveStorageConfig(ctx, credential, revision, next, actor, meta, "STORAGE_CREDENTIAL_REPLACE")
}

func (c *ControlPlane) DisableStorageCredential(ctx context.Context, id uuid.UUID, revision int64, actor domain.User, meta RequestMeta) (domain.StorageCredential, error) {
	if err := c.requireStorageAdmin(ctx, actor, meta); err != nil {
		return domain.StorageCredential{}, err
	}
	credential, err := c.store.StorageCredential(ctx, id)
	if err != nil {
		return domain.StorageCredential{}, err
	}
	if revision != credential.Revision {
		return domain.StorageCredential{}, domain.ErrRevisionConflict
	}
	credential.Status = "DISABLED"
	credential.UpdatedAt = c.clock().UTC()
	audit, err := c.userAudit("STORAGE_CREDENTIAL_DISABLE", "STORAGE_CREDENTIAL", id, actor.ID, meta, "CREDENTIAL_DISABLED")
	if err != nil {
		return domain.StorageCredential{}, err
	}
	return c.store.SaveStorageCredential(ctx, credential, revision, nil, audit)
}

func (c *ControlPlane) invalidStorageConfig(ctx context.Context, meta RequestMeta) error {
	if err := c.RecordDenied(ctx, "STORAGE_CREDENTIAL_CHANGE", "STORAGE_CREDENTIAL", "CONFIG_REJECTED", meta); err != nil {
		return err
	}
	return &ValidationError{Field: "rclone_config", Code: "CONFIG_REJECTED"}
}

func storageAAD(credential domain.StorageCredential, secretID uuid.UUID) []byte {
	return []byte(fmt.Sprintf("restfleet:storage-rclone:v1:%s:%d:%s:master:v1", credential.ID, credential.SecretRevision, secretID))
}

func (c *ControlPlane) saveStorageConfig(ctx context.Context, credential domain.StorageCredential, revision int64, config *rclone.Config, actor domain.User, meta RequestMeta, action string) (domain.StorageCredential, error) {
	secretID, err := uuid.NewV7()
	if err != nil {
		return domain.StorageCredential{}, err
	}
	plaintext := config.Bytes()
	defer clear(plaintext)
	sealed, err := security.SealEnvelope(c.masterKey, plaintext, storageAAD(credential, secretID))
	if err != nil {
		return domain.StorageCredential{}, err
	}
	envelope := domain.SecretEnvelope{
		ID: secretID, Kind: "RCLONE_CONFIG", Algorithm: security.EnvelopeAlgorithm, KeyID: "master:v1",
		Ciphertext: sealed.Ciphertext, Nonce: sealed.Nonce, WrappedDataKey: sealed.WrappedDataKey,
		WrapNonce: sealed.WrapNonce, AAD: sealed.AAD, CreatedAt: credential.UpdatedAt,
	}
	credential.SecretRef = secretID
	audit, err := c.userAudit(action, "STORAGE_CREDENTIAL", credential.ID, actor.ID, meta, "SECRET_CHANGED")
	if err != nil {
		return domain.StorageCredential{}, err
	}
	audit.Changes, err = json.Marshal(map[string]any{"secret": map[string]any{"changed": true, "revision": credential.SecretRevision}})
	if err != nil {
		return domain.StorageCredential{}, err
	}
	return c.store.SaveStorageCredential(ctx, credential, revision, &envelope, audit)
}

func openStorageSecret(key []byte, credential domain.StorageCredential, e domain.SecretEnvelope) ([]byte, error) {
	if e.ID != credential.SecretRef || e.Kind != "RCLONE_CONFIG" || e.KeyID != "master:v1" ||
		e.Algorithm != security.EnvelopeAlgorithm || !bytes.Equal(e.AAD, storageAAD(credential, e.ID)) ||
		len(e.Nonce) != 12 || len(e.WrapNonce) != 12 {
		return nil, domain.ErrStorageUnavailable
	}
	plaintext, err := security.OpenEnvelope(key, security.Envelope{Ciphertext: e.Ciphertext, Nonce: e.Nonce,
		WrappedDataKey: e.WrappedDataKey, WrapNonce: e.WrapNonce, AAD: e.AAD})
	if err != nil {
		return nil, domain.ErrStorageUnavailable
	}
	return plaintext, nil
}
