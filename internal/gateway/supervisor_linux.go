package gateway

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/sagehou/restfleet/internal/rclone"
)

var (
	ErrSupervisorClosed   = errors.New("gateway supervisor is closed")
	ErrSupervisorBusy     = errors.New("gateway capacity or repository credential is busy")
	ErrBackendUnavailable = errors.New("gateway backend unavailable")
	ErrBackupFailed       = errors.New("gateway backup callback failed")
	ErrGatewayAudit       = errors.New("gateway audit unavailable")
)

// BackupRequest is central-only borrowed secret material. The durable owner
// MUST validate Host/Repository/Credential bindings, audit secret access, acquire
// and renew the backup lease, and exclude maintenance/credential-test writers.
// It MUST keep those fences until WithBackup returns after ALL cleanup.
type BackupRequest struct {
	Binding      Binding
	CredentialID uuid.UUID
	Config       []byte
	Remote       string
}

// Access is borrowed only for the callback lifetime. Password is cleared after
// the callback returns. EndpointPath has no userinfo, secret, backend or socket.
// The owner supplies the independently verified TLS origin, stores runtime
// credentials safely and MUST NOT interpret delivery as an Agent ACK.
type Access struct {
	EndpointPath string
	Username     string
	Password     []byte
}

type activeBackup struct {
	binding      Binding
	credentialID uuid.UUID
	cancel       context.CancelFunc
	session      *BackupSession
}

// Supervisor is the in-process data-plane router and child owner. It is not a
// job queue or lease authority. Use ONE supervisor per gateway runtime; close
// the listener, then this supervisor, then the caller-owned credential runtime.
type Supervisor struct {
	mu      sync.Mutex
	runtime *rclone.Runtime
	limit   int
	audit   func(context.Context, Event) error
	active  map[uuid.UUID]*activeBackup
	closed  bool
	running sync.WaitGroup
}

func NewSupervisor(runtime *rclone.Runtime, maxSessions int, audit func(context.Context, Event) error) (*Supervisor, error) {
	if runtime == nil || maxSessions < 1 || maxSessions > 32 || audit == nil {
		return nil, ErrInvalidSession
	}
	return &Supervisor{runtime: runtime, limit: maxSessions, audit: audit, active: make(map[uuid.UUID]*activeBackup)}, nil
}

// WithBackup installs the route only after the private backend is ready, lends
// the session capability to run, then revokes, joins and removes all resources.
// run MUST honor ctx, join its work before returning, and never retain Access.
// persist and audit MUST durably commit and honor ctx; raw callback errors never
// escape. There is no retry/restart here: later attempts need fresh admission.
func (s *Supervisor) WithBackup(ctx context.Context, request BackupRequest,
	persist func(context.Context, []byte) error, run func(context.Context, Access) error,
) (result error) {
	if run == nil || persist == nil {
		return ErrInvalidSession
	}
	deadline, ok := ctx.Deadline()
	if !ok || time.Until(deadline) > 24*time.Hour {
		return rclone.ErrGatewayLifetime
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, id := range []uuid.UUID{request.Binding.HostID, request.Binding.RepositoryID, request.Binding.GatewayID, request.Binding.OperationID, request.CredentialID} {
		if id.Version() != 7 || id.Variant() != uuid.RFC4122 {
			return ErrInvalidSession
		}
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	entry := &activeBackup{binding: request.Binding, credentialID: request.CredentialID, cancel: cancel}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return ErrSupervisorClosed
	}
	busy := len(s.active) >= s.limit
	for _, active := range s.active {
		busy = busy || active.binding.RepositoryID == request.Binding.RepositoryID ||
			active.binding.HostID == request.Binding.HostID || active.binding.GatewayID == request.Binding.GatewayID ||
			active.binding.OperationID == request.Binding.OperationID || active.credentialID == request.CredentialID
	}
	if busy {
		s.mu.Unlock()
		return ErrSupervisorBusy
	}
	s.active[request.Binding.RepositoryID] = entry
	s.running.Add(1)
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.active, request.Binding.RepositoryID)
		s.mu.Unlock()
		s.running.Done()
	}()
	if !s.record(ctx, request.Binding, "session_start", "requested") {
		return ErrGatewayAudit
	}
	defer func() {
		// End auditing happens after runtime cleanup but before releasing capacity.
		// It cannot use an already canceled lease context or wait without a bound.
		if !s.record(context.WithoutCancel(ctx), request.Binding, "session_end", "finished") && result == nil {
			result = ErrGatewayAudit
		}
	}()
	var workErr error
	result = s.runtime.WithGatewayConfig(ctx, request.Config, request.Remote, persist,
		func(ctx context.Context, config, binary string) error {
			workErr = s.withBackend(ctx, entry, request.Remote, config, binary, run)
			return workErr
		})
	if errors.Is(result, rclone.ErrCommandFailed) {
		return workErr
	}
	return result
}

func (s *Supervisor) withBackend(ctx context.Context, entry *activeBackup, remote, config, binary string,
	run func(context.Context, Access) error,
) error {
	dir := filepath.Dir(config)
	socket := filepath.Join(dir, "rest.sock")
	if len(socket) >= 108 {
		return ErrInvalidSession
	}
	backendCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	root := remote + ":restfleet/agents/" + entry.binding.GatewayID.String() + "/" + entry.binding.RepositoryID.String()
	cmd := exec.CommandContext(backendCtx, binary, "serve", "restic", root,
		"--config", config, "--addr", socket, "--append-only", "--cache-objects=false",
		"--server-read-timeout", "1h", "--server-write-timeout", "1h", "--max-header-bytes", "16384",
		"--retries", "1", "--low-level-retries", "1", "--contimeout", "10s", "--timeout", "1h")
	cmd.Dir = dir
	cmd.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin", "LANG=C", "TMPDIR=" + dir}
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = 2 * time.Second
	if cmd.Start() != nil {
		return ErrBackendUnavailable
	}
	stopped := make(chan struct{})
	go func() { _ = cmd.Wait(); cancel(); close(stopped) }()
	defer func() { cancel(); <-stopped; _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }()
	if err := waitBackendSocket(backendCtx, socket); err != nil {
		return ErrBackendUnavailable
	}
	session, password, err := NewBackupSession(backendCtx, entry.binding, socket, s.audit)
	if err != nil {
		return err
	}
	defer clear(password)
	defer func() {
		// Unpublish before cancellation; already-routed requests are joined by Close.
		s.mu.Lock()
		entry.session = nil
		s.mu.Unlock()
		session.Close()
	}()
	probeCtx, stopProbe := context.WithTimeout(backendCtx, 15*time.Second)
	probe, err := session.request(probeCtx, http.MethodHead, "config", nil, nil)
	if err == nil {
		_ = probe.Body.Close()
	}
	stopProbe()
	if err != nil || probe.StatusCode != http.StatusOK || probe.ContentLength < 1 || probe.ContentLength > maxLockBytes {
		return ErrBackendUnavailable // Never initialize a missing repo or use a TCP fallback.
	}
	s.mu.Lock()
	if s.closed || backendCtx.Err() != nil {
		s.mu.Unlock()
		return ErrBackendUnavailable
	}
	entry.session = session
	s.mu.Unlock()
	err = run(backendCtx, Access{EndpointPath: session.prefix, Username: entry.binding.GatewayID.String(), Password: password})
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if backendCtx.Err() != nil {
		return ErrBackendUnavailable
	}
	if err != nil {
		return ErrBackupFailed
	}
	return nil
}

func waitBackendSocket(ctx context.Context, socket string) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		info, err := os.Lstat(socket)
		if err == nil {
			stat, ok := info.Sys().(*syscall.Stat_t)
			if info.Mode()&os.ModeSocket == 0 || !ok || stat.Uid != uint32(os.Geteuid()) || os.Chmod(socket, 0600) != nil {
				return ErrBackendUnavailable
			}
			return nil
		}
		if !os.IsNotExist(err) {
			return ErrBackendUnavailable
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (s *Supervisor) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// No ServeMux path cleaning or redirects: preserve raw input for the guard.
	var session *BackupSession
	s.mu.Lock()
	if !s.closed {
		// ponytail: at most 32 active routes; a routing tree is unnecessary here.
		for _, active := range s.active {
			if active.session != nil && strings.HasPrefix(r.URL.Path, active.session.prefix) {
				session = active.session
				break
			}
		}
	}
	s.mu.Unlock()
	if session != nil {
		session.ServeHTTP(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if !s.record(r.Context(), Binding{}, "denied", "route_unavailable") {
		http.Error(w, "gateway unavailable", http.StatusServiceUnavailable)
		return
	}
	http.Error(w, "gateway request rejected", http.StatusForbidden)
}

func (s *Supervisor) record(ctx context.Context, binding Binding, action, reason string) bool {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return s.audit(ctx, Event{Binding: binding, Action: action, Reason: reason}) == nil
}

func (s *Supervisor) Close() {
	s.mu.Lock()
	s.closed = true
	for _, active := range s.active {
		active.cancel()
	}
	s.mu.Unlock()
	s.running.Wait()
}
