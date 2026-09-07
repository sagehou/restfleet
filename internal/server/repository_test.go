package server

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sagehou/restfleet/internal/domain"
)

func TestRepositoryPasswordIsolation(t *testing.T) {
	c := &ControlPlane{masterKey: bytes.Repeat([]byte{4}, 32)}
	r := domain.Repository{ID: uuid.New(), HostID: uuid.New(), GatewaySecretRevision: 1, ResticSecretRevision: 1, CreatedAt: time.Now()}
	gateway, err := c.sealRepositoryPassword(r, "GATEWAY")
	if err != nil {
		t.Fatal(err)
	}
	restic, err := c.sealRepositoryPassword(r, "RESTIC_KEY")
	if err != nil {
		t.Fatal(err)
	}
	r.GatewaySecretRef, r.ResticSecretRef = gateway.ID, restic.ID
	raw, err := openRepositorySecret(c.masterKey, r, "GATEWAY", gateway)
	if err != nil || len(raw) != 43 {
		t.Fatal("gateway password invalid")
	}
	defer clear(raw)
	other, err := openRepositorySecret(c.masterKey, r, "RESTIC_KEY", restic)
	if err != nil || len(other) != 43 || bytes.Equal(raw, other) {
		t.Fatal("passwords are not independent")
	}
	defer clear(other)
	for _, change := range []func(*domain.Repository, *domain.SecretEnvelope){
		func(r *domain.Repository, _ *domain.SecretEnvelope) { r.ID = uuid.New() },
		func(r *domain.Repository, _ *domain.SecretEnvelope) { r.HostID = uuid.New() },
		func(r *domain.Repository, _ *domain.SecretEnvelope) { r.GatewaySecretRevision++ },
		func(r *domain.Repository, _ *domain.SecretEnvelope) { r.GatewaySecretRef = uuid.New() },
		func(_ *domain.Repository, e *domain.SecretEnvelope) { e.Kind = "REPOSITORY_RESTIC_KEY" },
		func(_ *domain.Repository, e *domain.SecretEnvelope) { e.Algorithm = "plaintext" },
		func(_ *domain.Repository, e *domain.SecretEnvelope) { e.KeyID = "master:v2" },
		func(_ *domain.Repository, e *domain.SecretEnvelope) { e.Ciphertext = []byte("tampered") },
		func(_ *domain.Repository, e *domain.SecretEnvelope) { e.Nonce = []byte{1} },
		func(_ *domain.Repository, e *domain.SecretEnvelope) { e.WrapNonce = []byte{1} },
		func(_ *domain.Repository, e *domain.SecretEnvelope) { e.AAD = []byte("tampered") },
	} {
		next, e := r, gateway
		change(&next, &e)
		if _, err := openRepositorySecret(c.masterKey, next, "GATEWAY", e); !errors.Is(err, domain.ErrStorageUnavailable) {
			t.Fatal("substituted envelope accepted")
		}
	}
	if _, err := openRepositorySecret(bytes.Repeat([]byte{9}, 32), r, "GATEWAY", gateway); !errors.Is(err, domain.ErrStorageUnavailable) {
		t.Fatal("wrong key accepted")
	}
	if _, err := openRepositorySecret(c.masterKey, r, "RCLONE_CONFIG", gateway); !errors.Is(err, domain.ErrStorageUnavailable) {
		t.Fatal("unknown kind accepted")
	}
}

func TestRepositoryCreateRequiresAdminAndKey(t *testing.T) {
	store := &storageDeniedStore{}
	c := &ControlPlane{store: store, clock: time.Now}
	if _, err := c.CreateRepository(context.Background(), "Repo", uuid.Nil, uuid.Nil, false, domain.User{Role: domain.RoleViewer}, RequestMeta{}); !errors.Is(err, ErrForbidden) || store.audits != 1 {
		t.Fatal("role denial was not audited")
	}
	if _, err := c.CreateRepository(context.Background(), "Repo", uuid.Nil, uuid.Nil, false, domain.User{Role: domain.RoleAdmin}, RequestMeta{}); !errors.Is(err, domain.ErrStorageUnavailable) {
		t.Fatal("missing key accepted")
	}
}
