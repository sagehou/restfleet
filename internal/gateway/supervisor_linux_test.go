package gateway

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sagehou/restfleet/internal/rclone"
)

func backupFixture() BackupRequest {
	return BackupRequest{Binding: bindingFixture(), CredentialID: uuid.MustParse("019abcde-1234-7000-8000-000000000005"), Remote: "encrypted",
		Config: []byte("[cloud]\ntype = onedrive\ndrive_id = fixture-drive\ndrive_type = personal\n" +
			"token = {\"access_token\":\"fixture-access\",\"token_type\":\"Bearer\",\"refresh_token\":\"fixture-refresh\",\"expiry\":\"2030-01-01T00:00:00Z\"}\n" +
			"[encrypted]\ntype = crypt\nremote = cloud:backups\npassword = AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA\n")}
}

func fakeGatewayExecutable(t *testing.T, mode, state string) string {
	t.Helper()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
	filename := filepath.Join(t.TempDir(), "gateway-engine")
	script := "#!/bin/sh\nexec " + quote(binary) + " -test.run=^TestGatewayBackendChild$ -- " + quote(mode) + " " + quote(state) + " \"$@\"\n"
	if err := os.WriteFile(filename, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return filename
}

// Shell is confined to this test shim. The production supervisor executes the
// fixed binary directly with argv and never accepts a command/flags from Agent.
func TestGatewayBackendChild(t *testing.T) {
	marker := -1
	for i, arg := range os.Args {
		if arg == "--" {
			marker = i
			break
		}
	}
	if marker < 0 {
		return
	}
	mode, state, args := os.Args[marker+1], os.Args[marker+2], os.Args[marker+3:]
	fail := func() { os.Exit(43) }
	want := []string{"serve", "restic", "", "--config", "", "--addr", "", "--append-only", "--cache-objects=false",
		"--server-read-timeout", "1h", "--server-write-timeout", "1h", "--max-header-bytes", "16384",
		"--retries", "1", "--low-level-retries", "1", "--contimeout", "10s", "--timeout", "1h"}
	if len(args) != len(want) {
		fail()
	}
	want[2], want[4], want[6] = args[2], args[4], args[6]
	if !reflect.DeepEqual(args, want) || filepath.Dir(args[4]) != filepath.Dir(args[6]) {
		fail()
	}
	parts := strings.Split(strings.TrimPrefix(args[2], "encrypted:restfleet/agents/"), "/")
	if len(parts) != 2 {
		fail()
	}
	for _, raw := range parts {
		id, err := uuid.Parse(raw)
		if err != nil || id.Version() != 7 {
			fail()
		}
	}
	for _, env := range os.Environ() {
		if strings.Contains(env, "parent-canary") || strings.HasPrefix(env, "RCLONE_") || strings.HasPrefix(env, "LISTEN_") ||
			strings.HasPrefix(env, "HTTP_PROXY=") || strings.HasPrefix(env, "HTTPS_PROXY=") || strings.HasPrefix(env, "ALL_PROXY=") {
			fail()
		}
	}
	config := args[4]
	info, err := os.Stat(config)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		fail()
	}
	raw, err := os.ReadFile(config)
	if err != nil || !bytes.Contains(raw, []byte("type = crypt")) {
		fail()
	}
	write := func(name string, data []byte) {
		if os.WriteFile(filepath.Join(state, name), data, 0600) != nil {
			fail()
		}
	}
	encoded, err := json.Marshal(args)
	if err != nil {
		fail()
	}
	write("args-"+parts[1], encoded)
	write("pid-"+parts[1], []byte(strconv.Itoa(os.Getpid())))
	if mode == "real" {
		binary, err := os.ReadFile(filepath.Join(state, "real-rclone"))
		if err != nil {
			fail()
		}
		// Test-only remote substitution; production ParseConfig never accepts local.
		args[2] = filepath.Join(state, "repo")
		if syscall.Exec(string(binary), append([]string{string(binary)}, args...), os.Environ()) != nil {
			fail()
		}
	}
	if mode == "exit" {
		_, _ = fmt.Fprint(os.Stderr, "fixture-access fixture-refresh provider-error-canary")
		os.Exit(90)
	}
	if mode == "unsafe-socket" {
		if os.WriteFile(args[6], []byte("not a socket"), 0600) != nil {
			fail()
		}
		select {}
	}
	child := exec.Command("/bin/sleep", "120")
	if child.Start() != nil {
		fail()
	}
	write("descendant-"+parts[1], []byte(strconv.Itoa(child.Process.Pid)))
	if mode == "never-ready" {
		_ = child.Wait()
		os.Exit(91)
	}
	listener, err := net.Listen("unix", args[6])
	if err != nil {
		fail()
	}
	server := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead && r.URL.Path == "/config" {
			if mode == "missing" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			if mode == "probe-failure" {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Length", "8")
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.URL.Path == "/config" {
			_, _ = fmt.Fprint(w, "fixture:"+parts[1])
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})}
	go func() {
		updated := false
		for {
			if _, err := os.Stat(filepath.Join(state, "terminate-"+parts[1])); err == nil {
				os.Exit(92)
			}
			if !updated && (mode == "refresh" || mode == "change-target") {
				if _, err := os.Stat(filepath.Join(state, "update-"+parts[1])); err == nil {
					next := bytes.ReplaceAll(raw, []byte("fixture-refresh"), []byte("refreshed-token"))
					if mode == "change-target" {
						next = bytes.ReplaceAll(raw, []byte("cloud:backups"), []byte("cloud:changed"))
					}
					if os.WriteFile(config+".new", next, 0600) != nil || os.Rename(config+".new", config) != nil {
						fail()
					}
					updated = true
				}
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	_ = server.Serve(listener)
	os.Exit(93)
}

func supervisorFixture(t *testing.T, mode string, limit int) (*Supervisor, string, string) {
	t.Helper()
	state := t.TempDir()
	root, err := os.MkdirTemp("/dev/shm", "rf-super-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	runtime, err := rclone.NewRuntime(root, fakeGatewayExecutable(t, mode, state))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	s, err := NewSupervisor(runtime, limit, acceptAudit)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s, state, root
}

type supervisedRun struct {
	ready   chan Access
	done    chan struct{}
	release chan struct{}
	cancel  context.CancelFunc
	err     error // Read only after done is closed.
	once    sync.Once
}

func startSupervised(t *testing.T, s *Supervisor, request BackupRequest, persist func(context.Context, []byte) error) *supervisedRun {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	op := &supervisedRun{ready: make(chan Access, 1), done: make(chan struct{}), release: make(chan struct{}), cancel: cancel}
	go func() {
		op.err = s.WithBackup(ctx, request, persist, func(ctx context.Context, access Access) error {
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

func waitAccess(t *testing.T, op *supervisedRun) Access {
	t.Helper()
	select {
	case access := <-op.ready:
		return access
	case <-op.done:
		t.Fatalf("session did not become ready: %v", op.err)
	case <-time.After(10 * time.Second):
		t.Fatal("session readiness hung")
	}
	return Access{}
}

func waitSupervised(t *testing.T, op *supervisedRun) {
	t.Helper()
	select {
	case <-op.done:
	case <-time.After(5 * time.Second):
		t.Fatal("supervisor cleanup hung")
	}
}

func finishSupervised(t *testing.T, op *supervisedRun) {
	t.Helper()
	op.once.Do(func() { close(op.release) })
	waitSupervised(t, op)
	if op.err != nil {
		t.Fatalf("supervisor failed: %v", op.err)
	}
}

func noGatewayRefresh(context.Context, []byte) error { return nil }

func supervisorRequest(s *Supervisor, access Access, path string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, access.EndpointPath+path, nil)
	r.TLS = &tls.ConnectionState{Version: tls.VersionTLS13}
	r.SetBasicAuth(access.Username, string(access.Password))
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func assertSupervisorClean(t *testing.T, state, root string, request BackupRequest) {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 1 || entries[0].Name() != ".lock" {
		t.Fatal("materialized runtime survived cleanup")
	}
	for _, prefix := range []string{"pid-", "descendant-"} {
		raw, err := os.ReadFile(filepath.Join(state, prefix+request.Binding.RepositoryID.String()))
		if os.IsNotExist(err) {
			continue
		}
		pid, parseErr := strconv.Atoi(string(raw))
		if err != nil || parseErr != nil || pid < 1 {
			t.Fatal("invalid process fixture")
		}
		for deadline := time.Now().Add(time.Second); ; {
			stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
			if os.IsNotExist(err) || bytes.Contains(stat, []byte(") Z ")) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("gateway process group survived cleanup")
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func TestSupervisorRoutesOnlyReadyScopedSessionsAndClearsSecrets(t *testing.T) {
	t.Setenv("RCLONE_CONFIG", "parent-canary-config")
	t.Setenv("RESTFLEET_MASTER_KEY", "parent-canary-master")
	t.Setenv("HTTP_PROXY", "http://parent-canary.invalid")
	t.Setenv("LISTEN_FDS", "parent-canary-descriptor")
	s, state, root := supervisorFixture(t, "success", 2)
	op := startSupervised(t, s, backupFixture(), noGatewayRefresh)
	access := waitAccess(t, op)
	w := supervisorRequest(s, access, "config")
	if w.Code != http.StatusOK || w.Body.String() != "fixture:"+backupFixture().Binding.RepositoryID.String() {
		t.Fatal("scoped route unavailable")
	}
	wrong := access
	wrong.EndpointPath = strings.Replace(access.EndpointPath, backupFixture().Binding.RepositoryID.String(), uuid.NewString(), 1)
	if supervisorRequest(s, wrong, "config").Code != http.StatusForbidden {
		t.Fatal("cross-repository path accepted")
	}
	if supervisorRequest(s, access, "../config").Code != http.StatusForbidden {
		t.Fatal("traversal was normalized")
	}
	oldSecret := bytes.Clone(access.Password)
	finishSupervised(t, op)
	if len(bytes.Trim(access.Password, "\x00")) != 0 {
		t.Fatal("borrowed capability not cleared")
	}
	access.Password = oldSecret
	if supervisorRequest(s, access, "config").Code != http.StatusForbidden {
		t.Fatal("finished route retained access")
	}
	assertSupervisorClean(t, state, root, backupFixture())
	// No ownership or capability is inherited by a later authorized attempt.
	next := backupFixture()
	next.Binding.OperationID = uuid.Must(uuid.NewV7())
	op = startSupervised(t, s, next, noGatewayRefresh)
	newAccess := waitAccess(t, op)
	if bytes.Equal(newAccess.Password, oldSecret) {
		t.Fatal("session capability reused")
	}
	if supervisorRequest(s, access, "config").Code != http.StatusUnauthorized {
		t.Fatal("old capability accepted by next attempt")
	}
	finishSupervised(t, op)
}

func TestSupervisorExcludesConflictsAndBoundsCapacity(t *testing.T) {
	s, _, _ := supervisorFixture(t, "success", 2)
	op := startSupervised(t, s, backupFixture(), noGatewayRefresh)
	_ = waitAccess(t, op)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for _, conflict := range []string{"host", "repository", "gateway", "operation", "credential"} {
		next := backupFixture()
		next.Binding.HostID, next.Binding.RepositoryID, next.Binding.GatewayID, next.Binding.OperationID, next.CredentialID = uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
		switch conflict {
		case "host":
			next.Binding.HostID = backupFixture().Binding.HostID
		case "repository":
			next.Binding.RepositoryID = backupFixture().Binding.RepositoryID
		case "gateway":
			next.Binding.GatewayID = backupFixture().Binding.GatewayID
		case "operation":
			next.Binding.OperationID = backupFixture().Binding.OperationID
		case "credential":
			next.CredentialID = backupFixture().CredentialID
		}
		if err := s.WithBackup(ctx, next, noGatewayRefresh, func(context.Context, Access) error { t.Error("conflicting callback ran"); return nil }); !errors.Is(err, ErrSupervisorBusy) {
			t.Fatal("conflicting admission accepted")
		}
	}
	next := backupFixture()
	next.Binding.HostID, next.Binding.RepositoryID, next.Binding.GatewayID, next.Binding.OperationID, next.CredentialID = uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	second := startSupervised(t, s, next, noGatewayRefresh)
	secondAccess := waitAccess(t, second)
	if supervisorRequest(s, secondAccess, "config").Body.String() != "fixture:"+next.Binding.RepositoryID.String() {
		t.Fatal("second backend routed incorrectly")
	}
	third := next
	third.Binding.HostID, third.Binding.RepositoryID, third.Binding.GatewayID, third.Binding.OperationID, third.CredentialID = uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	if err := s.WithBackup(ctx, third, noGatewayRefresh, func(context.Context, Access) error { return nil }); !errors.Is(err, ErrSupervisorBusy) {
		t.Fatal("capacity exceeded")
	}
	s.Close()
	waitSupervised(t, op)
	waitSupervised(t, second)
	if !errors.Is(op.err, context.Canceled) || !errors.Is(second.err, context.Canceled) {
		t.Fatal("shutdown did not cancel active sessions")
	}
	if err := s.WithBackup(ctx, next, noGatewayRefresh, func(context.Context, Access) error { return nil }); !errors.Is(err, ErrSupervisorClosed) {
		t.Fatal("closed supervisor restarted")
	}
}

func TestSupervisorBackendReadinessFailuresNeverPublish(t *testing.T) {
	for _, mode := range []string{"exit", "unsafe-socket", "missing", "probe-failure"} {
		t.Run(mode, func(t *testing.T) {
			s, state, root := supervisorFixture(t, mode, 1)
			op := startSupervised(t, s, backupFixture(), noGatewayRefresh)
			waitSupervised(t, op)
			if !errors.Is(op.err, ErrBackendUnavailable) {
				t.Fatalf("unexpected startup error: %v", op.err)
			}
			select {
			case <-op.ready:
				t.Fatal("failed backend published a capability")
			default:
			}
			assertSupervisorClean(t, state, root, backupFixture())
		})
	}
}

func TestSupervisorBackendDeathCancelsBorrowerAndReapsDescendants(t *testing.T) {
	s, state, root := supervisorFixture(t, "success", 1)
	op := startSupervised(t, s, backupFixture(), noGatewayRefresh)
	access := waitAccess(t, op)
	if err := os.WriteFile(filepath.Join(state, "terminate-"+backupFixture().Binding.RepositoryID.String()), nil, 0600); err != nil {
		t.Fatal(err)
	}
	waitSupervised(t, op)
	if !errors.Is(op.err, ErrBackendUnavailable) {
		t.Fatalf("backend death misclassified: %v", op.err)
	}
	if supervisorRequest(s, access, "config").Code != http.StatusForbidden {
		t.Fatal("dead backend remained routed")
	}
	assertSupervisorClean(t, state, root, backupFixture())
}

func TestSupervisorRefreshPersistsAndFailuresCancelWhileRunning(t *testing.T) {
	for _, mode := range []string{"refresh", "persist-fail", "change-target"} {
		t.Run(mode, func(t *testing.T) {
			engineMode := mode
			if mode == "persist-fail" {
				engineMode = "refresh"
			}
			s, state, root := supervisorFixture(t, engineMode, 1)
			refreshed := make(chan struct{}, 1)
			var borrowed []byte
			op := startSupervised(t, s, backupFixture(), func(_ context.Context, raw []byte) error {
				borrowed = raw
				if !bytes.Contains(raw, []byte("refreshed-token")) {
					t.Error("refresh contents missing")
				}
				refreshed <- struct{}{}
				if mode == "persist-fail" {
					return errors.New("provider-error-canary")
				}
				return nil
			})
			_ = waitAccess(t, op)
			if err := os.WriteFile(filepath.Join(state, "update-"+backupFixture().Binding.RepositoryID.String()), nil, 0600); err != nil {
				t.Fatal(err)
			}
			if mode == "refresh" {
				select {
				case <-refreshed:
				case <-time.After(5 * time.Second):
					t.Fatal("refresh not persisted while running")
				}
				finishSupervised(t, op)
			} else {
				waitSupervised(t, op)
				want := rclone.ErrRefreshPersist
				if mode == "change-target" {
					want = rclone.ErrConfigChanged
				}
				if !errors.Is(op.err, want) {
					t.Fatalf("refresh failure misclassified: %v", op.err)
				}
			}
			if len(bytes.Trim(borrowed, "\x00")) != 0 {
				t.Fatal("refresh plaintext retained")
			}
			assertSupervisorClean(t, state, root, backupFixture())
		})
	}
}

func TestSupervisorRejectsInvalidInputAndRedactsCallbacks(t *testing.T) {
	s, state, root := supervisorFixture(t, "success", 1)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	run := func(context.Context, Access) error { return errors.New("provider-error-canary") }
	if _, err := NewSupervisor(nil, 1, acceptAudit); err == nil {
		t.Fatal("nil runtime accepted")
	}
	if _, err := NewSupervisor(s.runtime, 0, acceptAudit); err == nil {
		t.Fatal("unbounded capacity accepted")
	}
	if _, err := NewSupervisor(s.runtime, 33, acceptAudit); err == nil {
		t.Fatal("excessive capacity accepted")
	}
	if _, err := NewSupervisor(s.runtime, 1, nil); err == nil {
		t.Fatal("missing audit accepted")
	}
	bad := backupFixture()
	bad.CredentialID = uuid.New()
	if err := s.WithBackup(ctx, bad, noGatewayRefresh, run); !errors.Is(err, ErrInvalidSession) {
		t.Fatal(err)
	}
	if err := s.WithBackup(ctx, backupFixture(), nil, run); !errors.Is(err, ErrInvalidSession) {
		t.Fatal(err)
	}
	if err := s.WithBackup(ctx, backupFixture(), noGatewayRefresh, nil); !errors.Is(err, ErrInvalidSession) {
		t.Fatal(err)
	}
	if err := s.WithBackup(context.Background(), backupFixture(), noGatewayRefresh, run); !errors.Is(err, rclone.ErrGatewayLifetime) {
		t.Fatal(err)
	}
	bad = backupFixture()
	bad.Config = []byte("invalid configuration")
	if err := s.WithBackup(ctx, bad, noGatewayRefresh, run); !errors.Is(err, rclone.ErrInvalidConfig) {
		t.Fatal(err)
	}
	s.audit = func(context.Context, Event) error { return errors.New("provider-error-canary") }
	if err := s.WithBackup(ctx, backupFixture(), noGatewayRefresh, run); !errors.Is(err, ErrGatewayAudit) {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(state)
	if err != nil || len(entries) != 0 {
		t.Fatal("invalid request started subprocess")
	}
	w := supervisorRequest(s, Access{EndpointPath: "/not-routed/"}, "config")
	if w.Code != http.StatusServiceUnavailable || strings.Contains(w.Body.String(), "canary") {
		t.Fatal("audit failure leaked or allowed access")
	}
	s.audit = acceptAudit
	if err := s.WithBackup(ctx, backupFixture(), noGatewayRefresh, run); !errors.Is(err, ErrBackupFailed) {
		t.Fatal(err)
	}
	assertSupervisorClean(t, state, root, backupFixture())
}
