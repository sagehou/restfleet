package gateway

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSupervisorCancellationDuringStartupDoesNotPublish(t *testing.T) {
	s, state, root := supervisorFixture(t, "never-ready", 1)
	op := startSupervised(t, s, backupFixture(), noGatewayRefresh)
	for deadline := time.Now().Add(5 * time.Second); ; {
		if _, err := os.Stat(filepath.Join(state, "descendant-"+backupFixture().Binding.RepositoryID.String())); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("startup child did not launch")
		}
		time.Sleep(10 * time.Millisecond)
	}
	op.cancel()
	waitSupervised(t, op)
	if !errors.Is(op.err, context.Canceled) {
		t.Fatalf("startup cancellation misclassified: %v", op.err)
	}
	select {
	case <-op.ready:
		t.Fatal("unready capability was published")
	default:
	}
	assertSupervisorClean(t, state, root, backupFixture())
}

func TestSupervisorEndAuditFailureStillCleansRuntime(t *testing.T) {
	s, state, root := supervisorFixture(t, "success", 1)
	s.audit = func(ctx context.Context, event Event) error {
		if event.Binding != backupFixture().Binding {
			t.Error("audit lost trusted binding")
		}
		if event.Action == "session_end" {
			deadline, ok := ctx.Deadline()
			if !ok || time.Until(deadline) > 3*time.Second {
				t.Error("end audit unbounded")
			}
			return errors.New("provider-error-canary")
		}
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	var borrowed []byte
	err := s.WithBackup(ctx, backupFixture(), noGatewayRefresh, func(_ context.Context, access Access) error {
		borrowed = access.Password
		return nil
	})
	if !errors.Is(err, ErrGatewayAudit) {
		t.Fatalf("end audit failed open: %v", err)
	}
	if len(bytes.Trim(borrowed, "\x00")) != 0 {
		t.Fatal("failed end audit retained capability")
	}
	assertSupervisorClean(t, state, root, backupFixture())
}
