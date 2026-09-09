package gateway

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sagehou/restfleet/internal/domain"
	"github.com/sagehou/restfleet/internal/persistence/postgres"
)

var _ AdmissionStore = (*postgres.Store)(nil)

type admissionStoreFixture struct {
	mu               sync.Mutex
	grant            domain.BackupAdmission
	err              error
	checks, releases int
	release          func(context.Context) error
}

func newAdmissionStoreFixture() *admissionStoreFixture {
	r := backupFixture()
	return &admissionStoreFixture{grant: domain.BackupAdmission{
		ID:     uuid.MustParse("019abcde-1234-7000-8000-000000000010"),
		Owner:  uuid.MustParse("019abcde-1234-7000-8000-000000000011"),
		HostID: r.Binding.HostID, RepositoryID: r.Binding.RepositoryID,
		GatewayID: r.Binding.GatewayID, StorageCredentialID: r.CredentialID,
		ConfigurationHash: strings.Repeat("a", 64), ExpiresAt: time.Now().Add(time.Minute),
	}}
}

func (f *admissionStoreFixture) CheckBackupAdmission(_ context.Context, id, owner uuid.UUID, hash string) (domain.BackupAdmission, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checks++
	if id != f.grant.ID || owner != f.grant.Owner || hash != f.grant.ConfigurationHash {
		return domain.BackupAdmission{}, domain.ErrBackupAdmission
	}
	return f.grant, f.err
}

func (f *admissionStoreFixture) ReleaseBackupAdmission(ctx context.Context, id, owner uuid.UUID) error {
	f.mu.Lock()
	f.releases++
	matches := id == f.grant.ID && owner == f.grant.Owner
	f.mu.Unlock()
	if !matches {
		return domain.ErrBackupAdmission
	}
	if f.release != nil {
		return f.release(ctx)
	}
	return nil
}

func startAdmitted(t *testing.T, s *Supervisor, f *admissionStoreFixture, persist func(context.Context, []byte) error) *supervisedRun {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	op := &supervisedRun{ready: make(chan Access, 1), done: make(chan struct{}), release: make(chan struct{}), cancel: cancel}
	id, owner, hash := f.grant.ID, f.grant.Owner, f.grant.ConfigurationHash
	go func() {
		op.err = s.WithAdmittedBackup(ctx, f, id, owner, hash, backupFixture(), persist,
			func(ctx context.Context, access Access) error {
				op.ready <- access
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-op.release:
					return nil
				}
			})
		close(op.done)
	}()
	t.Cleanup(func() { cancel(); waitSupervised(t, op) })
	return op
}

func TestAdmittedBackupReleasesAfterCleanupAndCloseJoinsRelease(t *testing.T) {
	s, state, root := supervisorFixture(t, "success", 1)
	f := newAdmissionStoreFixture()
	releasing, proceed := make(chan struct{}), make(chan struct{}, 1)
	defer close(proceed)
	f.release = func(ctx context.Context) error {
		close(releasing)
		select {
		case <-proceed:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	op := startAdmitted(t, s, f, noGatewayRefresh)
	access := waitAccess(t, op)
	oldSecret := bytes.Clone(access.Password)
	defer clear(oldSecret)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := s.WithAdmittedBackup(ctx, f, f.grant.ID, f.grant.Owner, f.grant.ConfigurationHash,
		backupFixture(), noGatewayRefresh, func(context.Context, Access) error { return nil }); !errors.Is(err, ErrSupervisorBusy) {
		t.Fatal("duplicate execution was admitted")
	}
	f.mu.Lock()
	releases := f.releases
	f.mu.Unlock()
	if releases != 0 {
		t.Fatal("duplicate touched the active owner's admission")
	}
	close(op.release)
	select {
	case <-releasing:
	case <-time.After(5 * time.Second):
		t.Fatal("release did not start")
	}
	assertSupervisorClean(t, state, root, backupFixture())
	if len(bytes.Trim(access.Password, "\x00")) != 0 {
		t.Fatal("capability survived release")
	}
	access.Password = oldSecret
	if supervisorRequest(s, access, "config").Code != http.StatusForbidden {
		t.Fatal("route survived release")
	}
	closed := make(chan struct{})
	go func() { s.Close(); close(closed) }()
	select {
	case <-closed:
		t.Fatal("Close returned before release finished")
	case <-time.After(30 * time.Millisecond):
	}
	// The release context is bounded and independent of Close's cancellation.
	proceed <- struct{}{}
	waitSupervised(t, op)
	if op.err != nil {
		t.Fatal(op.err)
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("Close did not join")
	}
	if f.releases != 1 {
		t.Fatal("release was not exactly once")
	}
}

func TestAdmittedBackupRejectsBindingsBeforeMaterialization(t *testing.T) {
	for _, field := range []string{"id", "owner", "hash", "host", "repository", "gateway", "credential", "expiry", "released", "database"} {
		t.Run(field, func(t *testing.T) {
			s, state, root := supervisorFixture(t, "success", 1)
			f := newAdmissionStoreFixture()
			id, owner, hash := f.grant.ID, f.grant.Owner, f.grant.ConfigurationHash
			switch field {
			case "id":
				id = uuid.Nil
			case "owner":
				owner = uuid.Nil
			case "hash":
				hash = strings.Repeat("A", 64)
			case "host":
				f.grant.HostID = uuid.Nil
			case "repository":
				f.grant.RepositoryID = uuid.Nil
			case "gateway":
				f.grant.GatewayID = uuid.Nil
			case "credential":
				f.grant.StorageCredentialID = uuid.Nil
			case "expiry":
				f.grant.ExpiresAt = time.Now().Add(-time.Second)
			case "released":
				now := time.Now()
				f.grant.ReleasedAt = &now
			case "database":
				f.err = errors.New("provider-secret-canary")
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			called := false
			err := s.WithAdmittedBackup(ctx, f, id, owner, hash, backupFixture(), noGatewayRefresh,
				func(context.Context, Access) error { called = true; return nil })
			if !errors.Is(err, ErrAdmissionUnavailable) || called || f.releases != 0 {
				t.Fatal("invalid admission was executed or released")
			}
			assertSupervisorClean(t, state, root, backupFixture())
			entries, err := os.ReadDir(state)
			if err != nil || len(entries) != 0 {
				t.Fatal("backend started before admission")
			}
		})
	}
}

func TestAdmittedBackupRevocationDeadlineAndRefreshFailureRetainFence(t *testing.T) {
	for _, mode := range []string{"revoked", "binding", "extended", "deadline", "cancel", "refresh", "end-audit", "release-error"} {
		t.Run(mode, func(t *testing.T) {
			engine := "success"
			if mode == "refresh" {
				engine = "refresh"
			}
			s, state, root := supervisorFixture(t, engine, 1)
			f := newAdmissionStoreFixture()
			if mode == "deadline" {
				f.grant.ExpiresAt = time.Now().Add(2 * time.Second)
			}
			if mode == "end-audit" {
				s.audit = func(_ context.Context, e Event) error {
					if e.Action == "session_end" {
						return errors.New("audit-secret-canary")
					}
					return nil
				}
			}
			if mode == "release-error" {
				f.release = func(context.Context) error { return errors.New("database-secret-canary") }
			}
			op := startAdmitted(t, s, f, func(context.Context, []byte) error { return errors.New("refresh-secret-canary") })
			_ = waitAccess(t, op)
			f.mu.Lock()
			switch mode {
			case "revoked":
				f.err = errors.New("database-secret-canary")
			case "binding":
				f.grant.HostID = uuid.Nil
			case "extended":
				f.grant.ExpiresAt = f.grant.ExpiresAt.Add(time.Hour)
			}
			f.mu.Unlock()
			switch mode {
			case "cancel":
				op.cancel()
			case "refresh":
				if err := os.WriteFile(filepath.Join(state, "update-"+backupFixture().Binding.RepositoryID.String()), nil, 0600); err != nil {
					t.Fatal(err)
				}
			case "end-audit", "release-error":
				close(op.release)
			}
			waitSupervised(t, op)
			if op.err == nil || strings.Contains(op.err.Error(), "canary") {
				t.Fatal("failure was ignored or leaked secrets")
			}
			want := 0
			if mode == "release-error" {
				want = 1
			}
			if f.releases != want {
				t.Fatal("uncertain cleanup released its fence")
			}
			assertSupervisorClean(t, state, root, backupFixture())
		})
	}
}
