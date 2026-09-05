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
