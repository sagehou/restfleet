package rclone

import (
	"context"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/webdav"
)

func publicLookup(context.Context, string) ([]netip.Addr, error) {
	return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
}

func TestWebDAVRuntimeRejectsDNSAndSocketChanges(t *testing.T) {
	root := tmpfsRoot(t)
	r, err := NewRuntime(root, fakeRclone(t, "success"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	for _, addresses := range [][]netip.Addr{nil, {netip.MustParseAddr("127.0.0.1")}, {netip.MustParseAddr("93.184.216.34"), netip.MustParseAddr("10.0.0.1")}} {
		r.lookupWebDAV = func(context.Context, string) ([]netip.Addr, error) { return addresses, nil }
		called := false
		err := r.WithConfig(context.Background(), []byte(webDAVConfig()), "encrypted", func(context.Context, []byte) error { return nil },
			func(context.Context, string, string) error { called = true; return nil })
		if !errors.Is(err, ErrUnsafeRuntime) || called {
			t.Fatal("unsafe DNS reached child execution")
		}
	}
	r.lookupWebDAV = publicLookup
	for _, mutation := range []string{"socket", "password"} {
		err := r.WithConfig(context.Background(), []byte(webDAVConfig()), "encrypted", func(context.Context, []byte) error { t.Error("WebDAV mutation persisted"); return nil },
			func(_ context.Context, filename, _ string) error {
				raw, err := os.ReadFile(filename)
				if err != nil {
					return err
				}
				if mutation == "socket" {
					raw = []byte(strings.Replace(string(raw), "webdav.sock", "other.sock", 1))
				} else {
					raw = []byte(strings.Replace(string(raw), "pass = ", "pass = AAAA", 1))
				}
				return os.WriteFile(filename, raw, 0600)
			})
		if !errors.Is(err, ErrConfigChanged) {
			t.Fatalf("accepted runtime %s change: %v", mutation, err)
		}
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 1 || entries[0].Name() != ".lock" {
		t.Fatal("runtime left plaintext/socket behind")
	}
}

func TestWebDAVRelayPinsDNSAndClosesActiveConnections(t *testing.T) {
	root := tmpfsRoot(t)
	r, err := NewRuntime(root, fakeRclone(t, "success"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	var lookups atomic.Int32
	r.lookupWebDAV = func(context.Context, string) ([]netip.Addr, error) {
		if lookups.Add(1) == 1 {
			return publicLookup(context.Background(), "")
		}
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	}
	peers := make(chan net.Conn, 2)
	r.dialWebDAV = func(_ context.Context, address string) (net.Conn, error) {
		if address != "93.184.216.34:443" {
			t.Error("destination was resolved again or changed")
		}
		a, b := net.Pipe()
		peers <- b
		return a, nil
	}
	config, _ := ParseConfig(webDAVConfig(), "encrypted")
	socket, closeRelay, err := r.webDAVRelay(context.Background(), config, root)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(socket)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("relay socket not private")
	}
	var clients, remotes []net.Conn
	for i := 0; i < 2; i++ {
		client, err := net.Dial("unix", socket)
		if err != nil {
			t.Fatal(err)
		}
		clients = append(clients, client)
		select {
		case peer := <-peers:
			remotes = append(remotes, peer)
		case <-time.After(time.Second):
			t.Fatal("relay not connected")
		}
	}
	closed := make(chan struct{})
	go func() { closeRelay(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("relay cleanup did not join connections")
	}
	for _, connection := range append(clients, remotes...) {
		_ = connection.SetReadDeadline(time.Now().Add(time.Second))
		if _, err := connection.Read(make([]byte, 1)); err == nil {
			t.Fatal("connection survived relay shutdown")
		}
		_ = connection.Close()
	}
	if lookups.Load() != 1 {
		t.Fatal("DNS pin not stable")
	}
}

func TestPinnedWebDAVTLSAndRedirectIsolation(t *testing.T) {
	binary := os.Getenv("RESTFLEET_TEST_RCLONE_BINARY")
	if binary == "" {
		t.Skip("CI supplies pinned rclone")
	}
	root := tmpfsRoot(t)
	files := t.TempDir()
	if err := os.Mkdir(filepath.Join(files, "backups"), 0700); err != nil {
		t.Fatal(err)
	}
	var mode atomic.Int32
	var escaped atomic.Int32
	forbidden := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) }))
	forbidden.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			escaped.Add(1)
		}
	}
	forbidden.Start()
	defer forbidden.Close()
	dav := &webdav.Handler{Prefix: "/", FileSystem: webdav.Dir(files), LockSystem: webdav.NewMemLS()}
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if mode.Load() == 1 {
			http.Redirect(w, req, strings.Replace(forbidden.URL, "http:", "https:", 1)+"/private", http.StatusFound)
			return
		}
		if mode.Load() == 2 {
			http.Redirect(w, req, forbidden.URL+"/private", http.StatusFound)
			return
		}
		user, _, ok := req.BasicAuth()
		if !ok || user != "test-user" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		dav.ServeHTTP(w, req)
	}))
	defer origin.Close()
	cert := origin.Certificate()
	if len(cert.DNSNames) == 0 {
		t.Fatal("test certificate must have a DNS identity")
	}
	_, port, _ := net.SplitHostPort(origin.Listener.Addr().String())
	endpoint := "https://" + net.JoinHostPort(cert.DNSNames[0], port) + "/"
	raw := strings.Replace(webDAVConfig(), "https://dav.example.test/files/", endpoint, 1)
	raw = strings.Replace(raw, "vendor = nextcloud", "vendor = other", 1)
	ca := filepath.Join(root, "test-ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	r, err := NewRuntime(root, binary)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	r.lookupWebDAV = publicLookup
	r.dialWebDAV = func(ctx context.Context, address string) (net.Conn, error) {
		if address != net.JoinHostPort("93.184.216.34", port) {
			t.Error("redirect escaped pinned destination")
		}
		return (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "tcp", origin.Listener.Addr().String())
	}
	run := func(trust bool) error {
		return r.WithConfig(context.Background(), []byte(raw), "encrypted", func(context.Context, []byte) error { t.Error("static credential refreshed"); return nil },
			func(ctx context.Context, filename, binary string) error {
				args := []string{"lsjson", "encrypted:", "--stat", "--config", filename, "--retries", "1", "--low-level-retries", "1", "--contimeout", "2s", "--timeout", "3s"}
				if trust {
					args = append(args, "--ca-cert", ca)
				}
				cmd := exec.CommandContext(ctx, binary, args...)
				cmd.Env = []string{"PATH=/usr/bin:/bin", "LANG=C", "TMPDIR=" + root}
				cmd.Stderr = io.Discard
				out, err := cmd.Output()
				if err != nil {
					return ErrTestFailed
				}
				if !strings.Contains(string(out), `"IsDir":true`) && !strings.Contains(string(out), `"IsDir": true`) {
					return ErrTestOutput
				}
				return nil
			})
	}
	if err := run(true); err != nil {
		t.Fatalf("pinned WebDAV TLS stat failed: %v", err)
	}
	if err := run(false); err == nil {
		t.Fatal("untrusted certificate accepted")
	}
	for _, redirectMode := range []int32{1, 2} {
		mode.Store(redirectMode)
		if err := run(true); err == nil {
			t.Fatal("unsafe redirect unexpectedly succeeded")
		}
	}
	if escaped.Load() != 0 {
		t.Fatal("redirect accessed forbidden endpoint")
	}
}

func TestGoogleRuntimeRefreshesOnlyTokens(t *testing.T) {
	root := tmpfsRoot(t)
	r, err := NewRuntime(root, fakeRclone(t, "success"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	var saved []byte
	err = r.WithConfig(context.Background(), []byte(googleConfig()), "encrypted",
		func(_ context.Context, raw []byte) error { saved = append([]byte(nil), raw...); return nil },
		func(_ context.Context, filename, _ string) error {
			raw, err := os.ReadFile(filename)
			if err != nil {
				return err
			}
			return os.WriteFile(filename, []byte(strings.Replace(string(raw), "canary-refresh", "google-refresh", 1)), 0600)
		})
	defer clear(saved)
	if err != nil || !strings.Contains(string(saved), "google-refresh") {
		t.Fatal("Google OAuth refresh was not persisted")
	}
}
