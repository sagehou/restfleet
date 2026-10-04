package gateway

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sagehou/restfleet/internal/domain"
	"github.com/sagehou/restfleet/internal/gatewaypending"
	"github.com/sagehou/restfleet/internal/security"
)

// This exercises the actual shipping engines without any cloud credentials.
// Only the remote is a local fixture; the frontend uses verified TLS and the
// backend is a private Unix socket. No Control API or gRPC proxy participates.
func TestPinnedGatewayBackupAndReadback(t *testing.T) {
	for _, policy := range []string{"online", "signed-local", "service"} {
		t.Run(policy, func(t *testing.T) { testPinnedGatewayBackupAndReadback(t, policy) })
	}
}

func testPinnedGatewayBackupAndReadback(t *testing.T, policy string) {
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
	var s *Supervisor
	var dir, runtimeRoot string
	var service *Service
	var serviceConfig ServiceConfig
	request := backupFixture()
	var local *AuthorizedBackup
	var queue *gatewaypending.Queue
	var central ed25519.PrivateKey
	var pendingSource ed25519.PublicKey
	var pendingPrivate []byte
	var server *publicFixture
	if policy == "service" {
		f := newServiceFixture(t, 1, "real")
		service = startServiceFixture(t, f)
		serviceConfig = f.config
		s, dir, runtimeRoot = service.supervisor, f.state, f.config.RuntimeDirectory
		b := f.config.Repositories[0].Binding
		request.Binding = Binding{HostID: b.HostID, RepositoryID: b.RepositoryID, GatewayID: b.GatewayID}
		local, queue, central, pendingSource, pendingPrivate = service.repositories[0].owner, service.repositories[0].queue, f.central, f.sources[0], f.pending
		server = &publicFixture{URL: "https://" + service.listener.Addr().String(), certificate: f.certificate, client: serviceTLSClient(t, f.certificate)}
	} else {
		s, dir, runtimeRoot = supervisorFixture(t, "real", 1)
	}
	if err := os.WriteFile(filepath.Join(dir, "real-rclone"), []byte(rcloneBinary), 0600); err != nil {
		t.Fatal(err)
	}
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
	var mu sync.Mutex
	lockCleanups := 0
	if service == nil {
		s.audit, err = NewAuditRecorder(auditStoreFunc(func(_ context.Context, e domain.AuditEvent) error {
		mu.Lock()
		defer mu.Unlock()
		if e.Action == "GATEWAY_LOCK_CLEANUP_INTENT" {
			lockCleanups++
		}
		return nil
		}))
		if err != nil {
			t.Fatal(err)
		}
	}
	cleanupSessions := make(map[uuid.UUID]bool)
	if policy == "signed-local" {
		var grant security.GatewayStatement
		_, _, grant, central, _, pendingPrivate = pendingGatewayFixture(t, gatewaypending.Limits{MaxBytes: 2 << 20, MaxRecords: 64})
		pendingRecipient, deriveErr := security.GatewayPendingPublicKey(pendingPrivate)
		if deriveErr != nil {
			t.Fatal(deriveErr)
		}
		local, queue, pendingSource = deliverGatewayOwner(t, s, grant, central, pendingRecipient)
		t.Cleanup(local.Close)
		// The authenticated Unix material channel initialized this owner. Its
		// central transaction is a fixture here; no central port is used by backup.
		s.audit = func(context.Context, Event) error { return ErrGatewayAudit }
	}
	if server == nil {
		server = startPublicFixture(t, s, nil)
	}
	admission := newAdmissionStoreFixture()
	start := func() *supervisedRun {
		if service != nil {
			return startServiceBackup(t, service, request.Binding.RepositoryID)
		}
		if local != nil {
			return startAuthorized(t, local, nil)
		}
		return startAdmitted(t, s, admission, noGatewayRefresh)
	}
	collectLocalAudits := func() {
		if local == nil {
			return
		}
		for _, record := range drainAuthorized(t, queue, central, pendingSource, pendingPrivate) {
			if record.Event != nil && record.Event.Action == "lock_cleanup" {
				cleanupSessions[record.Event.Binding.OperationID] = true
				mu.Lock()
				lockCleanups++
				mu.Unlock()
			}
		}
	}
	op := start()
	access := waitAccess(t, op)
	ca := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(ca, server.certificate, 0600); err != nil {
		t.Fatal(err)
	}
	env := []string{"RESTIC_REPOSITORY=rest:" + server.URL + access.EndpointPath, "RESTIC_REST_USERNAME=" + access.Username,
		"RESTIC_REST_PASSWORD=" + string(access.Password), "RESTIC_CACERT=" + ca}
	source := filepath.Join(dir, "source")
	if err := os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	filename, contents := "file ; $(not-a-command).txt", "gateway readback fixture\n"
	if err := os.WriteFile(filepath.Join(source, filename), []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	raw, err := run(env, "backup", "--host", request.Binding.HostID.String(), source)
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
	collectLocalAudits()
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
	if local != nil {
		finishSupervised(t, op)
		assertSupervisorClean(t, dir, runtimeRoot, request)
		op = start()
		access = waitAccess(t, op)
		env[2] = "RESTIC_REST_PASSWORD=" + string(access.Password)
		if os.WriteFile(filepath.Join(source, filename), []byte("second local backup fixture\n"), 0600) != nil {
			t.Fatal("second source")
		}
		if _, err := run(env, "backup", "--host", request.Binding.HostID.String(), source); err != nil {
			t.Fatal("second pinned backup under the same accepted grant failed")
		}
		raw, err = run(env, "snapshots")
		var second []struct {
			ID string `json:"id"`
		}
		if err != nil || json.Unmarshal(raw, &second) != nil || len(second) != 2 {
			t.Fatal("sequential backup snapshots missing")
		}
		raw, err = run(env, "dump", "latest", filepath.Join(source, filename))
		if err != nil || string(raw) != "second local backup fixture\n" {
			t.Fatal("second backup readback mismatch")
		}
		collectLocalAudits()
		if len(cleanupSessions) != 2 {
			t.Fatal("sequential sessions did not persist independent lock cleanup")
		}
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
	client := server.client
	call := func(method, path, body string, want int) {
		r, err := http.NewRequestWithContext(ctx, method, server.URL+access.EndpointPath+path, strings.NewReader(body))
		if err != nil {
			t.Fatal("invalid fixture request")
		}
		r.SetBasicAuth(access.Username, string(access.Password))
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
	finishSupervised(t, op)
	if local != nil {
		collectLocalAudits()
		if service != nil {
			if service.Close() != nil { t.Fatal("pinned service shutdown failed") }
			r := serviceConfig.Repositories[0]
			queue, err = gatewaypending.Recover(r.QueueDirectory, r.Binding, [32]byte(serviceConfig.RecipientPublic), pendingSource,
				central.Public().(ed25519.PublicKey), gatewaypending.Limits{MaxBytes: serviceConfig.MaxBytes, MaxRecords: serviceConfig.MaxRecords})
			if err != nil { t.Fatal("pinned service evidence recovery failed") }
			defer queue.Close()
		} else {
			local.Close()
		}
		if _, _, err := queue.Tail(); err != nil || admission.releases != 0 {
			t.Fatal("local cleanup released a central fence or lost pending data")
		}
	} else if admission.releases != 1 {
		t.Fatal("successful pinned backup did not release admission")
	}
	assertSupervisorClean(t, dir, runtimeRoot, request)
}
