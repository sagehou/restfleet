// Package gateway implements the data-plane boundary, not Control API routing.
package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/sagehou/restfleet/internal/security"
)

var ErrInvalidSession = errors.New("invalid gateway backup session")

const (
	maxObjectBytes = 32 << 20
	maxLockBytes   = 64 << 10
	maxLocks       = 1024
	protocolV2     = "application/vnd.x.restic.rest.v2"
)

var objectName = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Binding comes from the trusted session owner, never a request payload. The
// owner MUST admit/fence the durable backup lease and exclude central writes
// and other sessions on this repository for the entire session lifetime.
type Binding struct {
	HostID, RepositoryID, GatewayID, OperationID uuid.UUID
}

// Event contains only trusted route context and fixed classifications. Never add a
// URL, supplied username, Authorization, backend error or object content here.
type Event struct {
	Binding       Binding
	Authenticated bool
	Action        string
	Reason        string
}

// BackupSession is a single-use capability: it cannot be resumed after restart.
// It owns no provider credentials, Restic password, listeners or subprocesses.
// Close cancels and joins requests before the owner may replace the backend.
type BackupSession struct {
	binding   Binding
	prefix    string
	verifier  [sha256.Size]byte
	ctx       context.Context
	cancel    context.CancelFunc
	transport *http.Transport
	audit     func(context.Context, Event) error
	lifecycle sync.RWMutex
	requests  chan struct{}
	writes    chan struct{}
	locks     map[string]bool // Under writes: false means consumed or ambiguous.
}

// NewBackupSession requires a live, deadline-bound authorization and a private
// 0600 Unix socket in a canonical, service-owned 0700 directory. The owner MUST
// supply a repo-scoped append-only rclone on tmpfs, not the maintenance socket.
// The returned secret is one-time handoff material; never log it or use argv.
// Durable admission/audit and Agent delivery are supervisor responsibilities.
func NewBackupSession(ctx context.Context, binding Binding, socket string,
	audit func(context.Context, Event) error,
) (*BackupSession, []byte, error) {
	deadline, ok := ctx.Deadline()
	if !ok || !deadline.After(time.Now()) || ctx.Err() != nil || audit == nil {
		return nil, nil, ErrInvalidSession
	}
	for _, id := range []uuid.UUID{binding.HostID, binding.RepositoryID, binding.GatewayID, binding.OperationID} {
		if id.Version() != 7 || id.Variant() != uuid.RFC4122 {
			return nil, nil, ErrInvalidSession
		}
	}
	if !privateSocket(socket) {
		return nil, nil, ErrInvalidSession
	}
	token, err := security.NewOpaqueToken()
	if err != nil {
		return nil, nil, ErrInvalidSession
	}
	secret := []byte(token)
	ctx, cancel := context.WithCancel(ctx)
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	s := &BackupSession{
		binding: binding, prefix: "/restic/" + binding.GatewayID.String() + "/" + binding.RepositoryID.String() + "/",
		verifier: sha256.Sum256(secret), ctx: ctx, cancel: cancel, audit: audit,
		requests: make(chan struct{}, 8), writes: make(chan struct{}, 1), locks: make(map[string]bool),
		transport: &http.Transport{
			// No proxy, TCP fallback, redirect following or client-selected host.
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return dialer.DialContext(ctx, "unix", socket)
			},
			MaxConnsPerHost: 8, MaxIdleConnsPerHost: 8, IdleConnTimeout: time.Minute,
			ResponseHeaderTimeout: time.Hour, MaxResponseHeaderBytes: 16 << 10,
			DisableCompression: true,
		},
	}
	return s, secret, nil
}

func privateSocket(socket string) bool {
	if !filepath.IsAbs(socket) || filepath.Clean(socket) != socket || len(socket) >= 108 ||
		strings.ContainsAny(socket, "\x00\r\n") {
		return false
	}
	resolved, err := filepath.EvalSymlinks(socket)
	if err != nil || resolved != socket {
		return false
	}
	for _, path := range []string{filepath.Dir(socket), socket} {
		info, err := os.Lstat(path)
		if err != nil {
			return false
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != uint32(os.Geteuid()) {
			return false
		}
		if path == socket {
			if info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0600 {
				return false
			}
		} else if !info.IsDir() || info.Mode().Perm() != 0700 {
			return false
		}
	}
	return true
}

func (s *BackupSession) Close() {
	s.cancel()
	s.lifecycle.Lock()
	defer s.lifecycle.Unlock()
	s.transport.CloseIdleConnections()
	clear(s.verifier[:])
	clear(s.locks)
}

func (s *BackupSession) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.lifecycle.RLock()
	defer s.lifecycle.RUnlock()
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if s.ctx.Err() != nil {
		s.deny(w, r, false, "session_inactive", http.StatusForbidden)
		return
	}
	if r.TLS == nil || r.TLS.Version < tls.VersionTLS12 {
		s.deny(w, r, false, "tls_required", http.StatusForbidden)
		return
	}
	username, password, ok := r.BasicAuth()
	hash := sha256.Sum256([]byte(password))
	if !ok || len(r.Header.Values("Authorization")) != 1 || username != s.binding.GatewayID.String() ||
		subtle.ConstantTimeCompare(hash[:], s.verifier[:]) != 1 {
		w.Header().Set("WWW-Authenticate", `Basic realm="restfleet"`)
		s.deny(w, r, false, "authentication_failed", http.StatusUnauthorized)
		return
	}
	path, valid := s.objectPath(r)
	if !valid {
		s.deny(w, r, true, "path_rejected", http.StatusForbidden)
		return
	}
	select {
	case s.requests <- struct{}{}:
		defer func() { <-s.requests }()
	default:
		s.deny(w, r, true, "request_limit", http.StatusTooManyRequests)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Hour)
	stop := context.AfterFunc(s.ctx, cancel)
	defer func() { stop(); cancel() }()
	r = r.WithContext(ctx)
	deadline, _ := ctx.Deadline()
	_ = http.NewResponseController(w).SetReadDeadline(deadline)
	_ = http.NewResponseController(w).SetWriteDeadline(deadline)
	// A canceled session must also interrupt a stalled incoming upload, not just
	// upstream I/O. Join the callback before returning the ResponseWriter.
	interrupted := make(chan struct{})
	interrupt := context.AfterFunc(ctx, func() {
		_ = http.NewResponseController(w).SetReadDeadline(time.Now())
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now())
		close(interrupted)
	})
	defer func() {
		if !interrupt() {
			<-interrupted
		}
	}()
	if r.Method == http.MethodGet || (r.Method == http.MethodHead && !strings.HasSuffix(path, "/")) {
		s.read(w, r, path)
		return
	}
	if r.Method != http.MethodPost && r.Method != http.MethodDelete {
		s.deny(w, r, true, "method_rejected", http.StatusForbidden)
		return
	}
	if path == "config" || strings.HasPrefix(path, "keys/") || strings.HasSuffix(path, "/") ||
		(r.Method == http.MethodDelete && !strings.HasPrefix(path, "locks/")) {
		s.deny(w, r, true, "immutable_object", http.StatusForbidden)
		return
	}
	// ponytail: one bounded upload buffer and serialized mutations per session;
	// only introduce per-object parallelism after supervisor-wide resource limits.
	select {
	case s.writes <- struct{}{}:
		defer func() { <-s.writes }()
	case <-ctx.Done():
		http.Error(w, "gateway unavailable", http.StatusServiceUnavailable)
		return
	}
	if r.Method == http.MethodDelete {
		s.removeLock(w, r, path)
	} else {
		s.create(w, r, path)
	}
}

func (s *BackupSession) objectPath(r *http.Request) (string, bool) {
	u := r.URL
	if u.RawPath != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.User != nil ||
		u.Scheme != "" || u.Host != "" || u.Opaque != "" || !strings.HasPrefix(u.Path, s.prefix) {
		return "", false
	}
	path := strings.TrimPrefix(u.Path, s.prefix)
	if path == "config" {
		return path, true
	}
	kind, name, ok := strings.Cut(path, "/")
	if !ok || (kind != "data" && kind != "index" && kind != "snapshots" && kind != "keys" && kind != "locks") {
		return "", false
	}
	return path, name == "" || objectName.MatchString(name)
}

func (s *BackupSession) request(ctx context.Context, method, path string, body []byte, headers http.Header) (*http.Response, error) {
	u := &url.URL{Scheme: "http", Host: "gateway-backend.invalid", Path: "/" + path}
	r, err := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(body))
	if err != nil {
		return nil, ErrInvalidSession
	}
	r.Header.Set("Accept", protocolV2)
	if method == http.MethodPost {
		r.Header.Set("Content-Type", "application/octet-stream")
	}
	if method == http.MethodGet && headers.Get("Range") != "" {
		r.Header.Set("Range", headers.Get("Range"))
	}
	return s.transport.RoundTrip(r)
}

func (s *BackupSession) read(w http.ResponseWriter, r *http.Request, path string) {
	resp, err := s.request(r.Context(), r.Method, path, nil, r.Header)
	if err != nil {
		http.Error(w, "gateway unavailable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		// Restic treats ordinary list 404 as empty. Backend list errors must not
		// hide locks or snapshots, even when rclone misclassifies the failure.
		if strings.HasSuffix(path, "/") {
			resp.StatusCode = http.StatusBadGateway
		}
		s.backendFailure(w, resp.StatusCode)
		return
	}
	for _, key := range []string{"Content-Length", "Content-Range", "Accept-Ranges"} {
		if value := resp.Header.Get(key); value != "" {
			w.Header().Set(key, value)
		}
	}
	contentType := "application/octet-stream"
	if strings.HasSuffix(path, "/") {
		contentType = protocolV2
	}
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(resp.StatusCode)
	if r.Method != http.MethodHead {
		if _, err := io.Copy(w, resp.Body); err != nil {
			panic(http.ErrAbortHandler) // Never turn a truncated list/object into success.
		}
	}
}

func (s *BackupSession) create(w http.ResponseWriter, r *http.Request, path string) {
	lock := strings.HasPrefix(path, "locks/")
	limit := int64(maxObjectBytes)
	if lock {
		limit = maxLockBytes
		if _, seen := s.locks[path]; seen || len(s.locks) >= maxLocks {
			s.deny(w, r, true, "lock_not_fresh", http.StatusForbidden)
			return
		}
	}
	if r.ContentLength < 1 || r.ContentLength > limit {
		s.deny(w, r, true, "object_size", http.StatusRequestEntityTooLarge)
		return
	}
	body := make([]byte, int(r.ContentLength))
	defer clear(body)
	if _, err := io.ReadFull(r.Body, body); err != nil || r.Context().Err() != nil {
		s.deny(w, r, true, "invalid_body", http.StatusBadRequest)
		return
	}
	hash := sha256.Sum256(body)
	_, name, _ := strings.Cut(path, "/")
	if hex.EncodeToString(hash[:]) != name {
		s.deny(w, r, true, "content_hash_mismatch", http.StatusBadRequest)
		return
	}
	probe, err := s.request(r.Context(), http.MethodHead, path, nil, nil)
	if err != nil {
		http.Error(w, "gateway unavailable", http.StatusBadGateway)
		return
	}
	_ = probe.Body.Close()
	if probe.StatusCode != http.StatusNotFound {
		if probe.StatusCode == http.StatusOK {
			s.deny(w, r, true, "object_exists", http.StatusForbidden)
		} else {
			http.Error(w, "gateway unavailable", http.StatusBadGateway)
		}
		return
	}
	if lock {
		s.locks[path] = false // Reserve before sending; ambiguous writes never own locks.
	}
	resp, err := s.request(r.Context(), http.MethodPost, path, body, nil)
	if err != nil {
		http.Error(w, "gateway unavailable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		s.backendFailure(w, resp.StatusCode)
		return
	}
	if lock && r.Context().Err() == nil && s.ctx.Err() == nil {
		s.locks[path] = true
	}
	w.WriteHeader(http.StatusOK)
}

func (s *BackupSession) removeLock(w http.ResponseWriter, r *http.Request, path string) {
	if !s.locks[path] {
		s.deny(w, r, true, "lock_not_owned", http.StatusForbidden)
		return
	}
	if !s.record(r.Context(), true, "lock_cleanup", "owned_lock") {
		http.Error(w, "gateway unavailable", http.StatusServiceUnavailable)
		return
	}
	s.locks[path] = false // No replay after an uncertain delete, even if backend returns 404.
	resp, err := s.request(r.Context(), http.MethodDelete, path, nil, nil)
	if err != nil {
		http.Error(w, "gateway unavailable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		s.backendFailure(w, resp.StatusCode)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *BackupSession) record(ctx context.Context, authenticated bool, action, reason string) bool {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return s.audit(ctx, Event{Binding: s.binding, Authenticated: authenticated, Action: action, Reason: reason}) == nil
}

func (s *BackupSession) deny(w http.ResponseWriter, r *http.Request, authenticated bool, reason string, status int) {
	if !s.record(r.Context(), authenticated, "denied", reason) {
		http.Error(w, "gateway unavailable", http.StatusServiceUnavailable)
		return
	}
	http.Error(w, "gateway request rejected", status)
}

func (s *BackupSession) backendFailure(w http.ResponseWriter, status int) {
	if status != http.StatusNotFound && status != http.StatusRequestedRangeNotSatisfiable {
		status = http.StatusBadGateway
	}
	http.Error(w, "gateway request failed", status)
}
