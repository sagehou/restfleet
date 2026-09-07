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

func TestCredentialTestRequiresConfiguredRuntime(t *testing.T) {
	c := &ControlPlane{masterKey: bytes.Repeat([]byte{8}, 32), clock: time.Now}
	_, err := c.TestStorageCredential(context.Background(), uuid.Must(uuid.NewV7()), "key",
		domain.User{ID: uuid.Must(uuid.NewV7()), Role: domain.RoleAdmin}, RequestMeta{})
	if !errors.Is(err, domain.ErrStorageUnavailable) {
		t.Fatal("unconfigured worker accepted a job")
	}
	if _, err := c.ProcessCredentialJob(context.Background(), uuid.Must(uuid.NewV7())); !errors.Is(err, domain.ErrStorageUnavailable) {
		t.Fatal("unconfigured worker ran")
	}
}

func TestRepositoryInitializeRequiresRuntimeAndValidKey(t *testing.T) {
	c := &ControlPlane{masterKey: bytes.Repeat([]byte{8}, 32), clock: time.Now}
	admin := domain.User{Role: domain.RoleAdmin}
	if _, err := c.InitializeRepository(context.Background(), uuid.Must(uuid.NewV7()), "key", admin, RequestMeta{}); !errors.Is(err, domain.ErrStorageUnavailable) {
		t.Fatal("unconfigured initializer accepted work")
	}
	for _, key := range []string{"", "has space", "line\nfeed", string([]byte{127}), string(bytes.Repeat([]byte{'a'}, 129))} {
		if validateOperationKey(key) == nil {
			t.Fatal("invalid idempotency key accepted")
		}
	}
	if validateOperationKey("valid-key_123") != nil {
		t.Fatal("valid key rejected")
	}
}
