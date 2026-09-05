package server

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sagehou/restfleet/internal/domain"
	"github.com/sagehou/restfleet/internal/security"
)

func TestStorageEnvelopeBoundToCredentialAndRevision(t *testing.T) {
	key := bytes.Repeat([]byte{4}, 32)
	c := domain.StorageCredential{ID: uuid.New(), SecretRevision: 1, SecretRef: uuid.New()}
	sealed, err := security.SealEnvelope(key, []byte("test-sensitive-config"), storageAAD(c, c.SecretRef))
	if err != nil {
		t.Fatal(err)
	}
	e := domain.SecretEnvelope{ID: c.SecretRef, Kind: "RCLONE_CONFIG", Algorithm: security.EnvelopeAlgorithm, KeyID: "master:v1",
		Ciphertext: sealed.Ciphertext, Nonce: sealed.Nonce, WrappedDataKey: sealed.WrappedDataKey, WrapNonce: sealed.WrapNonce, AAD: sealed.AAD}
	plaintext, err := openStorageSecret(key, c, e)
	if err != nil || string(plaintext) != "test-sensitive-config" {
		t.Fatal("valid envelope failed")
	}
	clear(plaintext)
	for _, change := range []func(*domain.StorageCredential, *domain.SecretEnvelope){
		func(c *domain.StorageCredential, _ *domain.SecretEnvelope) { c.ID = uuid.New() },
		func(c *domain.StorageCredential, _ *domain.SecretEnvelope) { c.SecretRevision++ },
		func(c *domain.StorageCredential, _ *domain.SecretEnvelope) { c.SecretRef = uuid.New() },
		func(_ *domain.StorageCredential, e *domain.SecretEnvelope) { e.Kind = "AGENT_CA_PRIVATE_KEY" },
		func(_ *domain.StorageCredential, e *domain.SecretEnvelope) { e.KeyID = "master:v2" },
		func(_ *domain.StorageCredential, e *domain.SecretEnvelope) { e.Nonce = []byte{1} },
		func(_ *domain.StorageCredential, e *domain.SecretEnvelope) { e.WrapNonce = []byte{1} },
		func(_ *domain.StorageCredential, e *domain.SecretEnvelope) { e.Ciphertext = []byte{1} },
	} {
		other, next := c, e
		change(&other, &next)
		if _, err := openStorageSecret(key, other, next); !errors.Is(err, domain.ErrStorageUnavailable) {
			t.Fatal("tampered or substituted envelope was accepted")
		}
	}
}

type storageDeniedStore struct {
	Store
	audits int
}

func (s *storageDeniedStore) RecordAudit(context.Context, domain.AuditEvent) error {
	s.audits++
	return nil
}

func TestStorageMutationRequiresAdminAndConfiguredMasterKey(t *testing.T) {
	s := &storageDeniedStore{}
	c := &ControlPlane{store: s, clock: time.Now}
	meta := RequestMeta{RequestID: uuid.New()}
	viewer := domain.User{ID: uuid.New(), Role: domain.RoleViewer}
	if err := c.requireStorageAdmin(context.Background(), viewer, meta); !errors.Is(err, ErrForbidden) || s.audits != 1 {
		t.Fatal("unauthorized mutation was not denied and audited")
	}
	if err := c.requireStorageAdmin(context.Background(), domain.User{Role: domain.RoleAdmin}, meta); !errors.Is(err, domain.ErrStorageUnavailable) {
		t.Fatal("missing key did not fail closed")
	}
}
