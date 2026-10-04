package gateway

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sagehou/restfleet/internal/gatewaypending"
	"github.com/sagehou/restfleet/internal/security"
)

const (
	serviceGroup = 61000
	serviceCenterUID = 61001
	serviceGatewayUID = 61002
	serviceOtherUID = 61003
)

// Only the central child receives central secrets, through anonymous stdin.
// Gateway creates its own source/TLS keys and loads production metadata/pins.
// Central admission/registration transactions retain their separate DB tests.
type serviceIsolationConfig struct {
	Root string
	Runtime string
	Address string
	Binding security.GatewayAuthorizationBinding
	Origin security.GatewayAuditBinding
	Recipient []byte
	Source ed25519.PublicKey
	AuditSource ed25519.PublicKey
	CenterKey ed25519.PrivateKey
	PendingKey []byte
}

func TestCrossUIDServiceLifecycle(t *testing.T) {
	if os.Getenv("RESTFLEET_TEST_SERVICE_ISOLATION") != "1" { t.Skip("Actions explicitly enables isolated service orchestration") }
	if os.Geteuid() != 0 { t.Fatal("root coordinates only; services must run non-root") }
	root, err := os.MkdirTemp("", "rf-service-uid-")
	if err != nil || os.Chmod(root, 0755) != nil { t.Fatal("isolation root fixture") }
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	for _, dir := range []struct{ name string; uid int; mode os.FileMode }{
		{"gateway-trust", serviceGatewayUID, 0700}, {"gateway-metadata", serviceGatewayUID, 0700},
		{"gateway-queue", serviceGatewayUID, 0700}, {"gateway-audit", serviceGatewayUID, 0700}, {"gateway-state", serviceGatewayUID, 0700},
		{"gateway-ipc", serviceGatewayUID, 0710}, {"center-private", serviceCenterUID, 0700}, {"center-ipc", serviceCenterUID, 0710},
	} {
		path := filepath.Join(root, dir.name)
		if os.Mkdir(path, dir.mode) != nil || os.Chown(path, dir.uid, serviceGroup) != nil || os.Chmod(path, dir.mode) != nil { t.Fatal("isolated directory ownership") }
	}
	runtime, err := os.MkdirTemp("/dev/shm", "rf-service-uid-")
	if err != nil || os.Chown(runtime, serviceGatewayUID, serviceGroup) != nil { t.Fatal("isolated runtime") }
	t.Cleanup(func() { _ = os.RemoveAll(runtime) })
	public, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil { t.Fatal("central signing fixture") }
	defer clear(key)
	pending := make([]byte, 32)
	if _, err := rand.Read(pending); err != nil { t.Fatal("central recipient fixture") }
	defer clear(pending)
	recipient, err := security.GatewayPendingPublicKey(pending)
	if err != nil { t.Fatal("central recipient public key") }
	pin := filepath.Join(root, "gateway-trust", "center.pub")
	if os.WriteFile(pin, []byte(base64.StdEncoding.EncodeToString(public)), 0600) != nil || os.Chown(pin, serviceGatewayUID, serviceGroup) != nil { t.Fatal("independent central public pin") }
	reservation, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil { t.Fatal("public address fixture") }
	address := reservation.Addr().String()
	_ = reservation.Close()
	id := func() uuid.UUID { return uuid.Must(uuid.NewV7()) }
	binding := security.GatewayAuthorizationBinding{AdmissionID: id(), Owner: id(), RuntimeID: id(), AgentID: id(),
		HostID: id(), RepositoryID: id(), GatewayID: id(), StorageCredentialID: id(), DeliveryID: id(),
		GatewaySecretRef: id(), ResticSecretRef: id(), ConfigurationHash: strings.Repeat("a", 64)}
	c := serviceIsolationConfig{Root: root, Runtime: runtime, Address: address, Binding: binding,
		Origin: security.GatewayAuditBinding{OriginID: id(), RuntimeID: binding.RuntimeID}, Recipient: bytes.Clone(recipient[:])}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	start := func(role string, uid uint32, inputConfig serviceIsolationConfig) (io.WriteCloser, <-chan error) {
		t.Helper()
		binary, err := os.Executable()
		if err != nil { t.Fatal("isolated test executable") }
		cmd := exec.CommandContext(ctx, binary, "-test.run=^TestServiceIsolationChild$", "-test.count=1")
		cmd.Dir = root
		cmd.Env = []string{"RESTFLEET_SERVICE_ISOLATION_ROLE=" + role}
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uid, Gid: serviceGroup, Groups: []uint32{serviceGroup}}}
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr // Child assertions use fixed, secret-free classifications.
		input, err := cmd.StdinPipe()
		if err != nil || cmd.Start() != nil { t.Fatal("start non-root service child") }
		done := make(chan error, 1)
		go func() { done <- cmd.Wait(); close(done) }()
		t.Cleanup(func() { _ = input.Close(); _ = cmd.Process.Kill(); <-done })
		if json.NewEncoder(input).Encode(inputConfig) != nil { t.Fatal("anonymous isolated fixture input") }
		return input, done
	}
	wait := func(path string) {
		t.Helper()
		for {
			if _, err := os.Lstat(path); err == nil { return }
			select { case <-ctx.Done(): t.Fatal("isolated lifecycle fixture timed out"); case <-time.After(5*time.Millisecond): }
		}
	}
	finish := func(done <-chan error) {
		t.Helper()
		select { case err := <-done: if err != nil { t.Fatal("non-root service child failed") }; case <-ctx.Done(): t.Fatal("non-root service did not join") }
	}
	gatewayInput, gatewayDone := start("gateway", serviceGatewayUID, c)
	wait(filepath.Join(root, "gateway-ipc", "material.sock"))
	raw, err := os.ReadFile(filepath.Join(root, "gateway-ipc", "sources.json"))
	var pins struct{ Repository, Audit ed25519.PublicKey }
	if err != nil || json.Unmarshal(raw, &pins) != nil || len(pins.Repository) != 32 || len(pins.Audit) != 32 || bytes.Equal(pins.Repository, pins.Audit) { t.Fatal("Gateway-local independent sources") }
	c.Source, c.AuditSource = pins.Repository, pins.Audit
	centralConfig := c
	centralConfig.CenterKey, centralConfig.PendingKey = key, pending
	centerInput, centerDone := start("center", serviceCenterUID, centralConfig)
	wait(filepath.Join(root, "gateway-ipc", "installed"))
	otherInput, otherDone := start("other", serviceOtherUID, c)
	_ = otherInput.Close()
	finish(otherDone)
	if json.NewEncoder(centerInput).Encode(true) != nil { t.Fatal("renewal fixture trigger") }
	wait(filepath.Join(root, "center-ipc", "renewed"))
	if json.NewEncoder(gatewayInput).Encode(true) != nil { t.Fatal("TLS backup fixture trigger") }
	wait(filepath.Join(root, "gateway-ipc", "drained"))
	if json.NewEncoder(centerInput).Encode(true) != nil { t.Fatal("revocation fixture trigger") }
	finish(gatewayDone)
	_ = gatewayInput.Close()
	_ = centerInput.Close()
	finish(centerDone)
}

func TestServiceIsolationChild(t *testing.T) {
	role := os.Getenv("RESTFLEET_SERVICE_ISOLATION_ROLE")
	if role == "" { t.Skip("isolated child only") }
	if os.Geteuid() == 0 || os.Getegid() != serviceGroup { t.Fatal("service child lacks UID isolation") }
	decoder := json.NewDecoder(os.Stdin)
	var c serviceIsolationConfig
	if decoder.Decode(&c) != nil { t.Fatal("anonymous child fixture input") }
	defer clear(c.CenterKey)
	defer clear(c.PendingKey)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	path := func(dir, file string) string { return filepath.Join(c.Root, dir, file) }
	materialPath, authorityPath, replayPath := path("gateway-ipc", "material.sock"), path("gateway-ipc", "authority.sock"), path("center-ipc", "replay.sock")
	privateDenied := func(dir, file string) {
		t.Helper()
		if _, err := os.ReadFile(path(dir, file)); !errors.Is(err, os.ErrPermission) { t.Fatal("cross-service private file accessible") }
	}
	marker := func(dir, file string) {
		t.Helper()
		if os.WriteFile(path(dir, file), []byte("accepted"), 0644) != nil { t.Fatal("public lifecycle fixture marker") }
	}
	trigger := func() {
		t.Helper()
		var next bool
		if decoder.Decode(&next) != nil || !next { t.Fatal("trusted lifecycle fixture trigger") }
	}
	denied := func(context.Context) error { return nil }
	switch role {
	case "gateway":
		if os.Geteuid() != serviceGatewayUID || len(c.CenterKey) != 0 || len(c.PendingKey) != 0 { t.Fatal("central secret reached Gateway") }
		repoSource, err := security.CreateGatewaySource(path("gateway-trust", "repo.seed"))
		if err != nil { t.Fatal("local repository source creation") }
		auditSource, err := security.CreateGatewaySource(path("gateway-trust", "audit.seed"))
		if err != nil { t.Fatal("local independent audit source creation") }
		raw, err := json.Marshal(struct{ Repository, Audit ed25519.PublicKey }{repoSource, auditSource})
		if err != nil || os.WriteFile(path("gateway-ipc", "sources.json"), raw, 0644) != nil { t.Fatal("public source provisioning fixture") }
		cert, key := publicKeyPair(t, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
		if os.WriteFile(path("gateway-trust", "cert.pem"), cert, 0600) != nil || os.WriteFile(path("gateway-trust", "tls.key"), key, 0600) != nil { t.Fatal("local protected TLS material") }
		clear(key)
		state := path("gateway-state", "")
		config := ServiceConfig{Version: 1, Environment: "production", CentralPinFile: path("gateway-trust", "center.pub"),
			AuditOrigin: c.Origin, AuditSourceFile: path("gateway-trust", "audit.seed"), RecipientPublic: c.Recipient,
			AuditQueueDirectory: path("gateway-audit", ""), MaxBytes: 4<<20, MaxRecords: 128, RuntimeDirectory: c.Runtime,
			RcloneBinary: fakeGatewayExecutable(t, "success", state), CertificateFile: path("gateway-trust", "cert.pem"), TLSKeyFile: path("gateway-trust", "tls.key"),
			ListenAddress: c.Address, MaxSessions: 1, ServerUID: serviceCenterUID, SharedGroup: serviceGroup, ReplaySocket: replayPath, StartupWaitSeconds: 20,
			Repositories: []ServiceRepository{{Binding: c.Binding, SourceFile: path("gateway-trust", "repo.seed"), QueueDirectory: path("gateway-queue", ""), MaterialSocket: materialPath, AuthoritySocket: authorityPath}}}
		raw, err = json.Marshal(config)
		configPath := path("gateway-metadata", "service.json")
		if err != nil || os.WriteFile(configPath, raw, 0600) != nil { t.Fatal("protected production service metadata") }
		config, err = LoadServiceConfig(configPath)
		if err != nil { t.Fatal("production service metadata loader") }
		s, err := StartService(ctx, config)
		if err != nil { t.Fatal("production service lifecycle startup") }
		defer s.Close()
		privateDenied("center-private", "signing")
		privateDenied("center-private", "pending")
		if err := os.Remove(replayPath); !errors.Is(err, os.ErrPermission) { t.Fatal("Gateway could replace central socket") }
		owner := s.repositories[0].owner
		first := startServiceBackup(t, s, c.Binding.RepositoryID)
		access := waitAccess(t, first)
		marker("gateway-ipc", "installed")
		trigger()
		owner.authorization.mu.Lock()
		revision := owner.authorization.current.Revision
		owner.authorization.mu.Unlock()
		if revision != 2 || owner != s.repositories[0].owner { t.Fatal("renewal replaced the service owner") }
		client := serviceTLSClient(t, cert)
		call := func(access Access, requestPath string, status int) {
			t.Helper()
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+c.Address+requestPath, nil)
			if err != nil { t.Fatal("TLS fixture request") }
			req.SetBasicAuth(access.Username, string(access.Password))
			resp, err := client.Do(req)
			if err != nil { t.Fatal("verified isolated TLS request") }
			body, err := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if err != nil || resp.TLS == nil || resp.StatusCode != status || (status == 200 && string(body) != "fixture:"+c.Binding.RepositoryID.String()) { t.Fatal("isolated TLS identity or content mismatch") }
		}
		call(access, access.EndpointPath+"config", 200)
		call(access, "/metrics", 403)
		finishSupervised(t, first)
		second := startServiceBackup(t, s, c.Binding.RepositoryID)
		access = waitAccess(t, second)
		call(access, access.EndpointPath+"config", 200)
		finishSupervised(t, second)
		for {
			global, globalErr := s.globalQueue.Next()
			repo, repoErr := s.repositories[0].queue.Next()
			if globalErr != nil || repoErr != nil || !s.global.Ready() { t.Fatal("production service replay failed") }
			if global == nil && repo == nil { break }
			select { case <-ctx.Done(): t.Fatal("two-domain replay did not drain"); case <-time.After(10*time.Millisecond): }
		}
		active := startServiceBackup(t, s, c.Binding.RepositoryID)
		_ = waitAccess(t, active)
		marker("gateway-ipc", "drained")
		select { case <-s.done: case <-ctx.Done(): t.Fatal("isolated revocation did not join") }
		waitSupervised(t, active)
		if s.Wait() != ErrService || active.err != ErrService || owner.raw != nil || len(bytes.Trim(s.repositories[0].source, "\x00")) != 0 { t.Fatal("isolated revocation retained capability or material") }
		assertSupervisorClean(t, state, c.Runtime, BackupRequest{Binding: Binding{RepositoryID: c.Binding.RepositoryID}})
		q, err := gatewaypending.Recover(config.Repositories[0].QueueDirectory, c.Binding, [32]byte(c.Recipient), repoSource,
			owner.authorization.key, gatewaypending.Limits{MaxBytes: config.MaxBytes, MaxRecords: config.MaxRecords})
		if err != nil { t.Fatal("isolated shutdown lost encrypted repository evidence") }
		_ = q.Close()
	case "center":
		if os.Geteuid() != serviceCenterUID || len(c.CenterKey) != 64 || len(c.PendingKey) != 32 { t.Fatal("central fixture identity") }
		for name, secret := range map[string][]byte{"signing": c.CenterKey.Seed(), "pending": c.PendingKey} {
			file := path("center-private", name)
			if os.WriteFile(file, []byte(base64.StdEncoding.EncodeToString(secret)), 0600) != nil { t.Fatal("central protected fixture key") }
			loaded, err := security.ReadProtectedKey(file, 32)
			if err != nil || !bytes.Equal(secret, loaded) { t.Fatal("central protected fixture loading") }
			clear(loaded)
		}
		privateDenied("gateway-trust", "repo.seed")
		privateDenied("gateway-trust", "tls.key")
		privateDenied("gateway-metadata", "service.json")
		if err := os.Remove(materialPath); !errors.Is(err, os.ErrPermission) { t.Fatal("center could replace Gateway socket") }
		l, err := gatewaypending.ListenReplay(replayPath, serviceGroup)
		if err != nil { t.Fatal("isolated central replay listener") }
		var scoped, global, rejected atomic.Int32
		done := make(chan error, 1)
		go func() { done <- gatewaypending.ServeReplay(ctx, l, serviceGatewayUID, func(_ context.Context, runtime uuid.UUID, wire []byte) ([]byte, error) {
			if runtime != c.Binding.RuntimeID { return nil, gatewaypending.ErrChannel }
			r, err := security.OpenGatewayPending(wire, c.AuditSource, c.PendingKey)
			if err == nil && r.Header.AuditOrigin == c.Origin && r.Kind == "global_audit" {
				global.Add(1)
				return security.SignGatewayPendingReceipt(security.GatewayPendingReceipt{AuditOriginID: c.Origin.OriginID, RuntimeID: runtime,
					Sequence: r.Header.Sequence, RecordID: r.Header.RecordID, WireHash: security.GatewayPendingHash(wire)}, c.CenterKey)
			}
			r, err = security.OpenGatewayPending(wire, c.Source, c.PendingKey)
			if err != nil || r.Header.Binding != c.Binding || r.Kind != "audit" { return nil, gatewaypending.ErrChannel }
			scoped.Add(1)
			return security.SignGatewayPendingReceipt(security.GatewayPendingReceipt{AdmissionID: c.Binding.AdmissionID, RuntimeID: runtime,
				Sequence: r.Header.Sequence, RecordID: r.Header.RecordID, WireHash: security.GatewayPendingHash(wire)}, c.CenterKey)
		}, func(context.Context) error { rejected.Add(1); return nil }, serviceGroup) }()
		defer func() { cancel(); if <-done != nil { t.Error("isolated replay did not join") } }()
		f := serviceFixture{central: c.CenterKey, sources: []ed25519.PublicKey{c.Source}, config: ServiceConfig{RecipientPublic: c.Recipient, Repositories: []ServiceRepository{{Binding: c.Binding}}}}
		if gatewaypending.SendMaterial(ctx, materialPath, serviceGatewayUID, c.Binding, c.Source,
			func(_ context.Context, proof []byte) ([]byte, error) { return serviceMaterial(t, f, 0, proof), nil }, denied, serviceGroup) != nil { t.Fatal("isolated encrypted material delivery") }
		trigger()
		now := time.Now().Unix()
		grant, err := security.SignGatewayStatement(security.GatewayStatement{Binding: c.Binding, Revision: 2, IssuedAt: now-1, ExpiresAt: now+120}, c.CenterKey)
		if err != nil { t.Fatal("isolated renewal fixture") }
		for range 2 {
			if gatewaypending.SendAuthorization(ctx, authorityPath, serviceGatewayUID, c.Binding, c.Source,
				func(context.Context) ([]byte, error) { return grant, nil }, denied, serviceGroup) != nil { t.Fatal("isolated renewal and exact replay") }
		}
		marker("center-ipc", "renewed")
		trigger()
		revoke, err := security.SignGatewayStatement(security.GatewayStatement{Binding: c.Binding, Revision: 3, IssuedAt: time.Now().Unix(), Revoked: true}, c.CenterKey)
		if err != nil || gatewaypending.SendAuthorization(ctx, authorityPath, serviceGatewayUID, c.Binding, c.Source,
			func(context.Context) ([]byte, error) { return revoke, nil }, denied, serviceGroup) != nil { t.Fatal("isolated active revocation") }
		var shutdown bool
		if decoder.Decode(&shutdown) != io.EOF || scoped.Load() < 4 || global.Load() < 2 || rejected.Load() != 1 { t.Fatal("isolated replay or third UID boundary") }
	case "other":
		if os.Geteuid() != serviceOtherUID || len(c.CenterKey) != 0 || len(c.PendingKey) != 0 { t.Fatal("third UID fixture identity") }
		for _, pair := range [][2]string{{"gateway-trust", "repo.seed"}, {"gateway-trust", "tls.key"}, {"gateway-metadata", "service.json"}, {"gateway-queue", "identity"}, {"center-private", "signing"}, {"center-private", "pending"}} { privateDenied(pair[0], pair[1]) }
		if err := os.Remove(authorityPath); !errors.Is(err, os.ErrPermission) { t.Fatal("third UID could replace authority socket") }
		if _, err := gatewaypending.Replay(ctx, replayPath, serviceCenterUID, c.Binding.RuntimeID, []byte("untrusted-record"), serviceGroup); err != gatewaypending.ErrChannel { t.Fatal("third UID reached replay handler") }
		if gatewaypending.SendAuthorization(ctx, authorityPath, serviceGatewayUID, c.Binding, c.Source,
			func(context.Context) ([]byte, error) { t.Error("third UID reached decision callback"); return nil, gatewaypending.ErrChannel }, denied, serviceGroup) != gatewaypending.ErrChannel { t.Fatal("third UID reached service owner") }
	default: t.Fatal("unknown isolated service role")
	}
}
