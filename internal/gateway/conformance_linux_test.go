package gateway

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// This exercises the actual shipping engines without any cloud credentials.
// Only the remote is a local fixture; the frontend uses verified TLS and the
// backend is a private Unix socket. No Control API or gRPC proxy participates.
func TestPinnedGatewayBackupAndReadback(t *testing.T) {
	resticBinary, rcloneBinary := os.Getenv("RESTFLEET_TEST_RESTIC_BINARY"), os.Getenv("RESTFLEET_TEST_RCLONE_BINARY")
	if resticBinary == "" || rcloneBinary == "" {
		t.Skip("CI supplies pinned Restic and rclone executables")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	for binary, prefix := range map[string]string{resticBinary: "restic 0.19.1 ", rcloneBinary: "rclone v1.75.1"} {
		cmd := exec.CommandContext(ctx, binary, "version")
		cmd.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin", "LANG=C"}
		raw, err := cmd.Output()
		if err != nil || !strings.HasPrefix(string(raw), prefix) {
			t.Fatal("unapproved engine binary")
		}
	}
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	passwordFile := filepath.Join(dir, "password")
	if err := os.WriteFile(passwordFile, []byte("test-only-repository-password-for-gateway"), 0600); err != nil {
		t.Fatal(err)
	}
	baseEnv := []string{"PATH=/usr/local/bin:/usr/bin:/bin", "LANG=C", "TMPDIR=" + dir, "RESTIC_PASSWORD_FILE=" + passwordFile}
	run := func(env []string, args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, resticBinary, append([]string{"--json", "--no-cache"}, args...)...)
		cmd.Env = append(append([]string{}, baseEnv...), env...)
		// Raw subprocess errors/output are not printed on failure.
		cmd.Stderr = io.Discard
		return cmd.Output()
	}
	if _, err := run([]string{"RESTIC_REPOSITORY=" + repo}, "init", "--repository-version", "2"); err != nil {
		t.Fatal("fixture initialization failed")
	}
	initialConfig, err := os.ReadFile(filepath.Join(repo, "config"))
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := os.MkdirTemp("/dev/shm", "rf-gw-real-")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(runtime) }()
	config, socket := filepath.Join(runtime, "rclone.conf"), filepath.Join(runtime, "rest.sock")
	if err := os.WriteFile(config, nil, 0600); err != nil {
		t.Fatal(err)
	}
	backend := exec.CommandContext(ctx, rcloneBinary, "serve", "restic", repo,
		"--config", config, "--addr", socket, "--append-only", "--cache-objects=false",
		"--retries", "1", "--low-level-retries", "1")
	backend.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin", "LANG=C", "TMPDIR=" + runtime}
	backend.Stdout, backend.Stderr = io.Discard, io.Discard
	backend.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	if backend.Start() != nil {
		t.Fatal("fixture backend failed to start")
	}
	defer func() { _ = syscall.Kill(-backend.Process.Pid, syscall.SIGKILL); _ = backend.Wait() }()
	for deadline := time.Now().Add(10 * time.Second); ; {
		conn, err := net.DialTimeout("unix", socket, 100*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fixture backend socket unavailable")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := os.Chmod(socket, 0600); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	lockCleanups := 0
	s, secret, err := NewBackupSession(ctx, bindingFixture(), socket, func(_ context.Context, e Event) error {
		mu.Lock()
		defer mu.Unlock()
		if e.Action == "lock_cleanup" {
			lockCleanups++
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	server := httptest.NewTLSServer(s)
	defer server.Close()
	ca := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	env := []string{"RESTIC_REPOSITORY=rest:" + server.URL + s.prefix, "RESTIC_REST_USERNAME=" + s.binding.GatewayID.String(),
		"RESTIC_REST_PASSWORD=" + string(secret), "RESTIC_CACERT=" + ca}
	source := filepath.Join(dir, "source")
	if err := os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	filename, contents := "file ; $(not-a-command).txt", "gateway readback fixture\n"
	if err := os.WriteFile(filepath.Join(source, filename), []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	raw, err := run(env, "backup", "--host", s.binding.HostID.String(), source)
	if err != nil {
		t.Fatal("pinned backup through guarded TLS endpoint failed")
	}
	var summary bool
	for _, line := range strings.Split(string(raw), "\n") {
		var event struct {
			Type     string `json:"message_type"`
			Snapshot string `json:"snapshot_id"`
		}
		if json.Unmarshal([]byte(line), &event) == nil && event.Type == "summary" && event.Snapshot != "" {
			summary = true
		}
	}
	if !summary {
		t.Fatal("backup had no successful snapshot summary")
	}
	locks, err := os.ReadDir(filepath.Join(repo, "locks"))
	mu.Lock()
	cleaned := lockCleanups
	mu.Unlock()
	if err != nil || len(locks) != 0 || cleaned == 0 {
		t.Fatal("normal backup failed to clean its own lock")
	}
	raw, err = run(env, "snapshots")
	var snapshots []struct {
		ID string `json:"id"`
	}
	if err != nil || json.Unmarshal(raw, &snapshots) != nil || len(snapshots) != 1 || !objectName.MatchString(snapshots[0].ID) {
		t.Fatal("snapshot readback failed")
	}
	raw, err = run(env, "dump", snapshots[0].ID, filepath.Join(source, filename))
	if err != nil || string(raw) != contents {
		t.Fatal("encrypted backup readback mismatch")
	}
	// A wrong CA must fail with the same endpoint/auth; no insecure fallback.
	badCA := filepath.Join(dir, "bad-ca.pem")
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTemplate := &x509.Certificate{SerialNumber: big.NewInt(1), IsCA: true, BasicConstraintsValid: true,
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(badCA, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), 0600); err != nil {
		t.Fatal(err)
	}
	badEnv := append([]string{}, env...)
	badEnv[len(badEnv)-1] = "RESTIC_CACERT=" + badCA
	// Restic retries TLS failures. Bound this negative probe independently so
	// it cannot consume the lifetime of the remaining positive assertions.
	badCtx, stopBadProbe := context.WithTimeout(ctx, 5*time.Second)
	defer stopBadProbe()
	badCommand := exec.CommandContext(badCtx, resticBinary, "--json", "--no-cache", "snapshots")
	badCommand.Env = append(append([]string{}, baseEnv...), badEnv...)
	var tlsErrors bytes.Buffer
	badCommand.Stderr = &tlsErrors
	badErr := badCommand.Run()
	stopBadProbe()
	tlsRejected := bytes.Contains(tlsErrors.Bytes(), []byte("x509: certificate signed by unknown authority"))
	clear(tlsErrors.Bytes())
	if badErr == nil || !tlsRejected {
		t.Fatal("wrong CA did not produce an explicit trust verification failure")
	}
	client := server.Client()
	call := func(method, path, body string, want int) {
		r, err := http.NewRequestWithContext(ctx, method, server.URL+s.prefix+path, strings.NewReader(body))
		if err != nil {
			t.Fatal("invalid fixture request")
		}
		r.SetBasicAuth(s.binding.GatewayID.String(), string(secret))
		resp, err := client.Do(r)
		if err != nil {
			t.Fatal("gateway request failed")
		}
		defer resp.Body.Close()
		if resp.StatusCode != want {
			t.Fatalf("gateway conformance status=%d want=%d", resp.StatusCode, want)
		}
	}
	path := "snapshots/" + snapshots[0].ID
	call(http.MethodDelete, path, "", http.StatusForbidden)
	originalSnapshot, err := os.ReadFile(filepath.Join(repo, filepath.FromSlash(path)))
	if err != nil {
		t.Fatal(err)
	}
	call(http.MethodPost, path, string(originalSnapshot), http.StatusForbidden)
	call(http.MethodPost, path, "corruption attempt", http.StatusBadRequest)
	call(http.MethodPost, "config", string(initialConfig), http.StatusForbidden)
	call(http.MethodDelete, "config", "", http.StatusForbidden)
	// A central/pre-existing lock must survive, even though raw rclone permits it.
	foreign := "preexisting-central-lock-fixture"
	hash := sha256.Sum256([]byte(foreign))
	foreignPath := "locks/" + hex.EncodeToString(hash[:])
	if err := os.WriteFile(filepath.Join(repo, filepath.FromSlash(foreignPath)), []byte(foreign), 0600); err != nil {
		t.Fatal(err)
	}
	call(http.MethodDelete, foreignPath, "", http.StatusForbidden)
	call(http.MethodPost, foreignPath, foreign, http.StatusForbidden)
	after, err := os.ReadFile(filepath.Join(repo, "config"))
	if err != nil || string(after) != string(initialConfig) {
		t.Fatal("config changed")
	}
	after, err = os.ReadFile(filepath.Join(repo, filepath.FromSlash(path)))
	if err != nil || string(after) != string(originalSnapshot) {
		t.Fatal("snapshot changed")
	}
	after, err = os.ReadFile(filepath.Join(repo, filepath.FromSlash(foreignPath)))
	if err != nil || string(after) != foreign {
		t.Fatal("foreign lock changed")
	}
}
