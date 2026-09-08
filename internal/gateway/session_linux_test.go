package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

func bindingFixture() Binding {
	return Binding{
		HostID:       uuid.MustParse("019abcde-1234-7000-8000-000000000001"),
		RepositoryID: uuid.MustParse("019abcde-1234-7000-8000-000000000002"),
		GatewayID:    uuid.MustParse("019abcde-1234-7000-8000-000000000003"),
		OperationID:  uuid.MustParse("019abcde-1234-7000-8000-000000000004"),
	}
}

func object(kind, content string) string {
	hash := sha256.Sum256([]byte(content))
	return kind + "/" + hex.EncodeToString(hash[:])
}

func socketServer(t *testing.T, handler http.Handler) string {
	t.Helper()
	dir, err := os.MkdirTemp("/dev/shm", "rf-gw-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "rest.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(socket, 0600); err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: handler, ReadHeaderTimeout: time.Second}
	stopped := make(chan struct{})
	go func() { _ = server.Serve(listener); close(stopped) }()
	t.Cleanup(func() { _ = server.Close(); <-stopped })
	return socket
}

// Deliberately full-access: the boundary must protect objects even when a
// backend would accept a forbidden method. Pinned conformance adds append-only.
type fakeBackend struct {
	mu                                   sync.Mutex
	objects                              map[string]string
	calls                                []string
	headStatus, postStatus, deleteStatus int
	leaked                               bool
}

func (b *fakeBackend) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b.mu.Lock()
	defer b.mu.Unlock()
	path := strings.TrimPrefix(r.URL.Path, "/")
	b.calls = append(b.calls, r.Method+" "+path)
	for _, key := range []string{"Authorization", "Cookie", "Forwarded", "X-Forwarded-Host", "X-Original-URL", "X-HTTP-Method-Override"} {
		b.leaked = b.leaked || r.Header.Get(key) != ""
	}
	status := http.StatusOK
	value, exists := b.objects[path]
	switch r.Method {
	case http.MethodHead:
		if !exists {
			status = http.StatusNotFound
		}
		if b.headStatus != 0 {
			status = b.headStatus
		}
		w.WriteHeader(status)
	case http.MethodGet:
		if strings.HasSuffix(path, "/") {
			w.Header().Set("Content-Type", protocolV2)
			_, _ = io.WriteString(w, "[]")
		} else if exists {
			_, _ = io.WriteString(w, value)
		} else {
			http.Error(w, "provider-error-canary", http.StatusNotFound)
		}
	case http.MethodPost:
		raw, _ := io.ReadAll(r.Body)
		b.objects[path] = string(raw)
		if b.postStatus != 0 {
			status = b.postStatus
		}
		w.Header().Set("Location", "https://provider-error-canary.invalid")
		w.Header().Set("Set-Cookie", "provider-error-canary")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, "provider-error-canary")
	case http.MethodDelete:
		delete(b.objects, path)
		if b.deleteStatus != 0 {
			status = b.deleteStatus
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, "provider-error-canary")
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

type backendSnapshot struct {
	objects map[string]string
	calls   []string
	leaked  bool
}

func (b *fakeBackend) inspect() backendSnapshot {
	b.mu.Lock()
	defer b.mu.Unlock()
	result := backendSnapshot{objects: make(map[string]string), calls: append([]string{}, b.calls...), leaked: b.leaked}
	for key, value := range b.objects {
		result.objects[key] = value
	}
	return result
}

func (b *fakeBackend) change(fn func(*fakeBackend)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	fn(b)
}

func newSession(t *testing.T, socket string, binding Binding, audit func(context.Context, Event) error) (*BackupSession, []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	s, secret, err := NewBackupSession(ctx, binding, socket, audit)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s, secret
}

func acceptAudit(context.Context, Event) error { return nil }

func fixture(t *testing.T) (*BackupSession, []byte, *fakeBackend) {
	t.Helper()
	b := &fakeBackend{objects: map[string]string{"config": "encrypted-config"}}
	s, secret := newSession(t, socketServer(t, b), bindingFixture(), acceptAudit)
	return s, secret, b
}

func request(s *BackupSession, secret []byte, method, path, content string) *http.Request {
	r := httptest.NewRequest(method, s.prefix+path, strings.NewReader(content))
	r.TLS = &tls.ConnectionState{Version: tls.VersionTLS13}
	r.SetBasicAuth(s.binding.GatewayID.String(), string(secret))
	return r
}

func serve(t *testing.T, s *BackupSession, r *http.Request, want int) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != want {
		t.Fatalf("status=%d want=%d", w.Code, want)
	}
	if strings.Contains(w.Body.String(), "provider-error-canary") ||
		strings.Contains(fmt.Sprint(w.Header()), "provider-error-canary") {
		t.Fatal("backend response leaked")
	}
	return w
}

func TestSessionPathAuthenticationAndMethodBoundary(t *testing.T) {
	for name, change := range map[string]func(*http.Request){
		"no TLS":       func(r *http.Request) { r.TLS = nil },
		"obsolete TLS": func(r *http.Request) { r.TLS.Version = tls.VersionTLS11 },
		"cross repo": func(r *http.Request) {
			r.URL.Path = strings.Replace(r.URL.Path, bindingFixture().RepositoryID.String(), uuid.NewString(), 1)
		},
		"cross host": func(r *http.Request) {
			r.URL.Path = strings.Replace(r.URL.Path, bindingFixture().GatewayID.String(), uuid.NewString(), 1)
		},
		"traversal":         func(r *http.Request) { r.URL.Path += "/../config" },
		"encoded traversal": func(r *http.Request) { r.URL.RawPath = r.URL.Path + "%2f.." },
		"query":             func(r *http.Request) { r.URL.RawQuery = "create=true" },
		"empty query":       func(r *http.Request) { r.URL.ForceQuery = true },
		"absolute URI":      func(r *http.Request) { r.URL.Host = "attacker.invalid"; r.URL.Scheme = "https" },
		"nul":               func(r *http.Request) { r.URL.Path += "\x00" },
		"unknown file":      func(r *http.Request) { r.URL.Path = strings.Replace(r.URL.Path, "config", "rclone.conf", 1) },
		"double slash":      func(r *http.Request) { r.URL.Path = strings.Replace(r.URL.Path, "/config", "//config", 1) },
		"put":               func(r *http.Request) { r.Method = http.MethodPut },
		"patch":             func(r *http.Request) { r.Method = http.MethodPatch },
		"connect":           func(r *http.Request) { r.Method = http.MethodConnect },
	} {
		t.Run(name, func(t *testing.T) {
			s, secret, b := fixture(t)
			r := request(s, secret, http.MethodGet, "config", "")
			change(r)
			serve(t, s, r, http.StatusForbidden)
			if len(b.inspect().calls) != 0 {
				t.Fatal("rejected request reached backend")
			}
		})
	}
	for _, mode := range []string{"missing", "wrong", "other user", "duplicate"} {
		t.Run(mode, func(t *testing.T) {
			s, secret, b := fixture(t)
			r := request(s, secret, http.MethodGet, "config", "")
			switch mode {
			case "missing":
				r.Header.Del("Authorization")
			case "wrong":
				r.SetBasicAuth(s.binding.GatewayID.String(), "untrusted-password")
			case "other user":
				r.SetBasicAuth(uuid.NewString(), string(secret))
			case "duplicate":
				r.Header.Add("Authorization", r.Header.Get("Authorization"))
			}
			serve(t, s, r, http.StatusUnauthorized)
			if len(b.inspect().calls) != 0 {
				t.Fatal("unauthorized request reached backend")
			}
		})
	}
}

func TestSessionReadCreateAndOwnLockRefresh(t *testing.T) {
	s, secret, b := fixture(t)
	r := request(s, secret, http.MethodGet, "config", "")
	for _, key := range []string{"Cookie", "Forwarded", "X-Forwarded-Host", "X-Original-URL", "X-HTTP-Method-Override"} {
		r.Header.Set(key, "untrusted-canary")
	}
	w := serve(t, s, r, http.StatusOK)
	if w.Body.String() != "encrypted-config" || b.inspect().leaked {
		t.Fatal("read or header isolation failed")
	}
	serve(t, s, request(s, secret, http.MethodGet, "index/", ""), http.StatusOK)
	for _, kind := range []string{"data", "index", "snapshots", "locks"} {
		content := "encrypted-" + kind
		path := object(kind, content)
		serve(t, s, request(s, secret, http.MethodPost, path, content), http.StatusOK)
		serve(t, s, request(s, secret, http.MethodGet, path, ""), http.StatusOK)
		serve(t, s, request(s, secret, http.MethodPost, path, content), http.StatusForbidden)
		if kind != "locks" {
			serve(t, s, request(s, secret, http.MethodDelete, path, ""), http.StatusForbidden)
		}
	}
	old := object("locks", "encrypted-locks")
	next := object("locks", "refreshed-lock")
	serve(t, s, request(s, secret, http.MethodPost, next, "refreshed-lock"), http.StatusOK)
	serve(t, s, request(s, secret, http.MethodDelete, old, ""), http.StatusOK)
	serve(t, s, request(s, secret, http.MethodDelete, next, ""), http.StatusOK)
	serve(t, s, request(s, secret, http.MethodDelete, next, ""), http.StatusForbidden)
	serve(t, s, request(s, secret, http.MethodPost, next, "refreshed-lock"), http.StatusForbidden)
	if _, exists := b.inspect().objects[old]; exists {
		t.Fatal("old own lock not cleaned")
	}
}

func TestSessionNeverGrantsMaintenanceOrPreexistingLockOwnership(t *testing.T) {
	s, secret, b := fixture(t)
	for _, kind := range []string{"config", "keys", "data", "index", "snapshots", "locks"} {
		path, content := object(kind, "existing"), "existing"
		if kind == "config" {
			path = "config"
		}
		b.change(func(b *fakeBackend) { b.objects[path] = content })
		serve(t, s, request(s, secret, http.MethodPost, path, content), http.StatusForbidden)
		serve(t, s, request(s, secret, http.MethodDelete, path, ""), http.StatusForbidden)
		if b.inspect().objects[path] != content {
			t.Fatal("existing object changed")
		}
	}
	for _, path := range []string{"", "data/", "locks/", "keys/"} {
		serve(t, s, request(s, secret, http.MethodPost, path, ""), http.StatusForbidden)
	}
	serve(t, s, request(s, secret, http.MethodPost, object("keys", "new-key"), "new-key"), http.StatusForbidden)
}

func TestSessionValidatesContentBeforeBackendAndBoundsMemory(t *testing.T) {
	s, secret, b := fixture(t)
	path := object("data", "expected")
	serve(t, s, request(s, secret, http.MethodPost, path, "tampered"), http.StatusBadRequest)
	for _, size := range []int64{-1, 0, maxObjectBytes + 1} {
		r := request(s, secret, http.MethodPost, path, "short")
		r.ContentLength = size
		serve(t, s, r, http.StatusRequestEntityTooLarge)
	}
	r := request(s, secret, http.MethodPost, object("locks", "short"), "short")
	r.ContentLength = maxLockBytes + 1
	serve(t, s, r, http.StatusRequestEntityTooLarge)
	r = request(s, secret, http.MethodPost, path, "short")
	r.ContentLength = 20
	serve(t, s, r, http.StatusBadRequest)
	if len(b.inspect().calls) != 0 {
		t.Fatal("unvalidated body reached backend")
	}
	for i := range maxLocks {
		s.locks[fmt.Sprint(i)] = false
	}
	serve(t, s, request(s, secret, http.MethodPost, object("locks", "new"), "new"), http.StatusForbidden)
}

func TestSessionSerializesDuplicateUploads(t *testing.T) {
	s, secret, b := fixture(t)
	var wg sync.WaitGroup
	statuses := make(chan int, 4)
	for range 4 {
		wg.Go(func() {
			w := httptest.NewRecorder()
			s.ServeHTTP(w, request(s, secret, http.MethodPost, object("data", "same"), "same"))
			statuses <- w.Code
		})
	}
	wg.Wait()
	close(statuses)
	success := 0
	for status := range statuses {
		if status == http.StatusOK {
			success++
		} else if status != http.StatusForbidden {
			t.Fatal("unexpected duplicate result")
		}
	}
	if success != 1 || len(b.inspect().objects) != 2 {
		t.Fatal("concurrent duplicate overwrote object")
	}
}

func TestSessionAmbiguousBackendFailuresFailClosed(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusInternalServerError, http.StatusFound} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			s, secret, b := fixture(t)
			b.change(func(b *fakeBackend) { b.headStatus = status })
			serve(t, s, request(s, secret, http.MethodPost, object("data", "new"), "new"), http.StatusBadGateway)
			if len(b.inspect().calls) != 1 || len(b.inspect().objects) != 1 {
				t.Fatal("uncertain existence allowed upload")
			}
		})
	}
	t.Run("uncertain upload", func(t *testing.T) {
		s, secret, b := fixture(t)
		b.change(func(b *fakeBackend) { b.postStatus = http.StatusInternalServerError })
		path := object("locks", "new")
		serve(t, s, request(s, secret, http.MethodPost, path, "new"), http.StatusBadGateway)
		serve(t, s, request(s, secret, http.MethodDelete, path, ""), http.StatusForbidden)
		serve(t, s, request(s, secret, http.MethodPost, path, "new"), http.StatusForbidden)
		if b.inspect().objects[path] != "new" {
			t.Fatal("uncertain lock was removed")
		}
	})
	t.Run("uncertain delete", func(t *testing.T) {
		s, secret, b := fixture(t)
		path := object("locks", "new")
		serve(t, s, request(s, secret, http.MethodPost, path, "new"), http.StatusOK)
		b.change(func(b *fakeBackend) { b.deleteStatus = http.StatusInternalServerError })
		serve(t, s, request(s, secret, http.MethodDelete, path, ""), http.StatusBadGateway)
		b.change(func(b *fakeBackend) { b.objects[path] = "replacement" })
		serve(t, s, request(s, secret, http.MethodDelete, path, ""), http.StatusForbidden)
		if b.inspect().objects[path] != "replacement" {
			t.Fatal("delete replay changed replacement")
		}
	})
}

func TestSessionAuditFailureAndRedaction(t *testing.T) {
	s, secret, b := fixture(t)
	path := object("locks", "owned")
	serve(t, s, request(s, secret, http.MethodPost, path, "owned"), http.StatusOK)
	s.audit = func(context.Context, Event) error { return errors.New("provider-error-canary") }
	serve(t, s, request(s, secret, http.MethodDelete, path, ""), http.StatusServiceUnavailable)
	if b.inspect().objects[path] != "owned" {
		t.Fatal("unaudited deletion occurred")
	}
	var events []Event
	s.audit = func(_ context.Context, e Event) error { events = append(events, e); return nil }
	r := request(s, secret, http.MethodGet, "../provider-error-canary", "")
	serve(t, s, r, http.StatusForbidden)
	serve(t, s, request(s, secret, http.MethodDelete, path, ""), http.StatusOK)
	raw, err := json.Marshal(events)
	if err != nil || len(events) != 2 || events[1].Action != "lock_cleanup" ||
		bytes.Contains(raw, secret) || bytes.Contains(raw, []byte("provider-error-canary")) {
		t.Fatal("invalid or sensitive audit event")
	}
}

func TestSessionRestartDoesNotInheritCapabilityOrLocks(t *testing.T) {
	s, secret, b := fixture(t)
	path := object("locks", "leftover")
	serve(t, s, request(s, secret, http.MethodPost, path, "leftover"), http.StatusOK)
	s.Close()
	serve(t, s, request(s, secret, http.MethodDelete, path, ""), http.StatusForbidden)
	binding := bindingFixture()
	binding.OperationID = uuid.Must(uuid.NewV7())
	next, nextSecret := newSession(t, socketServer(t, b), binding, acceptAudit)
	if bytes.Equal(secret, nextSecret) {
		t.Fatal("backup capability reused")
	}
	serve(t, next, request(next, secret, http.MethodGet, "config", ""), http.StatusUnauthorized)
	serve(t, next, request(next, nextSecret, http.MethodDelete, path, ""), http.StatusForbidden)
	if b.inspect().objects[path] != "leftover" {
		t.Fatal("new session deleted old lock")
	}
}

func TestSessionCloseCancelsAndJoinsBackendRequest(t *testing.T) {
	started := make(chan struct{})
	socket := socketServer(t, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	}))
	s, secret := newSession(t, socket, bindingFixture(), acceptAudit)
	done := make(chan struct{})
	go func() {
		s.ServeHTTP(httptest.NewRecorder(), request(s, secret, http.MethodGet, "config", ""))
		close(done)
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("backend did not start")
	}
	closed := make(chan struct{})
	go func() { s.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("session close hung")
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("close did not join request")
	}
}

func TestSessionRejectsUnsafeConstructor(t *testing.T) {
	socket := socketServer(t, &fakeBackend{objects: map[string]string{}})
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for _, badSocket := range []string{"relative.sock", "/missing/rest.sock", filepath.Join(filepath.Dir(socket), "..", "rest.sock")} {
		if _, _, err := NewBackupSession(ctx, bindingFixture(), badSocket, acceptAudit); !errors.Is(err, ErrInvalidSession) {
			t.Fatal("unsafe socket accepted")
		}
	}
	link := filepath.Join(filepath.Dir(socket), "alias.sock")
	if err := os.Symlink(socket, link); err != nil {
		t.Fatal(err)
	}
	if _, _, err := NewBackupSession(ctx, bindingFixture(), link, acceptAudit); err == nil {
		t.Fatal("socket symlink accepted")
	}
	if err := os.Chmod(socket, 0666); err != nil {
		t.Fatal(err)
	}
	if _, _, err := NewBackupSession(ctx, bindingFixture(), socket, acceptAudit); err == nil {
		t.Fatal("public socket accepted")
	}
	if err := os.Chmod(socket, 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := NewBackupSession(context.Background(), bindingFixture(), socket, acceptAudit); err == nil {
		t.Fatal("unbounded session accepted")
	}
	if _, _, err := NewBackupSession(ctx, bindingFixture(), socket, nil); err == nil {
		t.Fatal("missing audit accepted")
	}
	bad := bindingFixture()
	bad.OperationID = uuid.New()
	if _, _, err := NewBackupSession(ctx, bad, socket, acceptAudit); err == nil {
		t.Fatal("invalid binding accepted")
	}
	cancel()
	if _, _, err := NewBackupSession(ctx, bindingFixture(), socket, acceptAudit); err == nil {
		t.Fatal("expired session accepted")
	}
}
