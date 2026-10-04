package gateway

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sagehou/restfleet/internal/gatewaypending"
	"github.com/sagehou/restfleet/internal/rclone"
	"github.com/sagehou/restfleet/internal/security"
)

type serviceFixture struct {
	config ServiceConfig
	central ed25519.PrivateKey
	sources []ed25519.PublicKey
	auditSource ed25519.PublicKey
	pending []byte
	state string
	certificate []byte
}

func newServiceFixture(t *testing.T, count int, mode string) serviceFixture {
	t.Helper()
	base, err := os.MkdirTemp("", "rf-service-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	private := func(name string) string {
		t.Helper()
		path := filepath.Join(base, name)
		if os.Mkdir(path, 0700) != nil {
			t.Fatal("service private directory")
		}
		return path
	}
	runtime, err := os.MkdirTemp("/dev/shm", "rf-service-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(runtime) })
	trust := private("trust")
	metadata := private("metadata")
	ipc := private("ipc")
	centralPublic, central, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { clear(central) })
	pending := make([]byte, 32)
	if _, err := rand.Read(pending); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { clear(pending) })
	recipient, err := security.GatewayPendingPublicKey(pending)
	if err != nil {
		t.Fatal(err)
	}
	write := func(name string, raw []byte) string {
		t.Helper()
		path := filepath.Join(trust, name)
		if os.WriteFile(path, raw, 0600) != nil {
			t.Fatal("service protected trust fixture")
		}
		return path
	}
	f := serviceFixture{central: central, pending: pending, state: private("engine-state")}
	cert, key := publicKeyPair(t, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	defer clear(key)
	f.certificate = cert
	f.config = ServiceConfig{Version: 1, Environment: "test", CentralPinFile: write("central.pub", []byte(base64.StdEncoding.EncodeToString(centralPublic))),
		AuditOrigin: security.GatewayAuditBinding{OriginID: uuid.Must(uuid.NewV7()), RuntimeID: uuid.Must(uuid.NewV7())},
		AuditSourceFile: filepath.Join(trust, "audit.seed"), RecipientPublic: bytes.Clone(recipient[:]), AuditQueueDirectory: private("audit-queue"),
		MaxBytes: 4 << 20, MaxRecords: 128, RuntimeDirectory: runtime, RcloneBinary: fakeGatewayExecutable(t, mode, f.state),
		CertificateFile: write("public.pem", cert), TLSKeyFile: write("tls.key", key), ListenAddress: "127.0.0.1:0", MaxSessions: 2,
		ServerUID: uint32(os.Geteuid()), ReplaySocket: filepath.Join(private("center-ipc"), "replay.sock"), StartupWaitSeconds: 10}
	f.auditSource, err = security.CreateGatewaySource(f.config.AuditSourceFile)
	if err != nil {
		t.Fatal(err)
	}
	for range count {
		b := security.GatewayAuthorizationBinding{AdmissionID: uuid.Must(uuid.NewV7()), Owner: uuid.Must(uuid.NewV7()), RuntimeID: f.config.AuditOrigin.RuntimeID,
			AgentID: uuid.Must(uuid.NewV7()), HostID: uuid.Must(uuid.NewV7()), RepositoryID: uuid.Must(uuid.NewV7()), GatewayID: uuid.Must(uuid.NewV7()),
			StorageCredentialID: uuid.Must(uuid.NewV7()), DeliveryID: uuid.Must(uuid.NewV7()), GatewaySecretRef: uuid.Must(uuid.NewV7()),
			ResticSecretRef: uuid.Must(uuid.NewV7()), ConfigurationHash: strings.Repeat("a", 64)}
		label := uuid.Must(uuid.NewV7()).String()
		r := ServiceRepository{Binding: b, SourceFile: filepath.Join(trust, label+".seed"), QueueDirectory: private(label),
			MaterialSocket: filepath.Join(ipc, label+".m"), AuthoritySocket: filepath.Join(ipc, label+".a")}
		public, err := security.CreateGatewaySource(r.SourceFile)
		if err != nil {
			t.Fatal(err)
		}
		f.sources = append(f.sources, public)
		f.config.Repositories = append(f.config.Repositories, r)
	}
	raw, err := json.Marshal(f.config)
	path := filepath.Join(metadata, "service.json")
	if err != nil || os.WriteFile(path, raw, 0600) != nil {
		t.Fatal("service metadata fixture")
	}
	f.config, err = LoadServiceConfig(path)
	if err != nil {
		t.Fatal("service protected configuration")
	}
	return f
}

func serviceMaterial(t *testing.T, f serviceFixture, index int, proof []byte) []byte {
	t.Helper()
	r := f.config.Repositories[index]
	challenge, err := security.VerifyGatewayMaterialChallenge(proof, f.sources[index], r.Binding)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	statement, err := security.SignGatewayStatement(security.GatewayStatement{Binding: r.Binding, Revision: 1, IssuedAt: now - 1, ExpiresAt: now + 120}, f.central)
	if err != nil {
		t.Fatal(err)
	}
	config, err := rclone.ParseConfig(string(backupFixture().Config), "encrypted")
	if err != nil {
		t.Fatal(err)
	}
	raw := config.Bytes()
	defer clear(raw)
	wire, err := security.SealGatewayMaterial(security.GatewayMaterial{Challenge: challenge, Source: f.sources[index],
		PendingRecipient: [32]byte(f.config.RecipientPublic), Statement: statement, AdmissionCreatedAt: now - 2, AdmissionExpiresAt: now + 180,
		SecretRevision: 1, Remote: "encrypted", ConfigHash: security.GatewayPendingHash(raw), Config: raw}, f.central)
	if err != nil {
		t.Fatal(err)
	}
	return wire
}

func waitServiceSocket(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSocket != 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("service did not bind initialization socket")
		}
		time.Sleep(time.Millisecond)
	}
}

func startServiceFixture(t *testing.T, f serviceFixture) *Service {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	type result struct { service *Service; err error }
	done := make(chan result, 1)
	go func() { s, err := StartService(ctx, f.config); done <- result{s, err} }()
	for i, r := range f.config.Repositories {
		waitServiceSocket(t, r.MaterialSocket)
		err := gatewaypending.SendMaterial(ctx, r.MaterialSocket, uint32(os.Geteuid()), r.Binding, f.sources[i],
			func(_ context.Context, proof []byte) ([]byte, error) { return serviceMaterial(t, f, i, proof), nil }, func(context.Context) error { return nil })
		if err != nil {
			cancel()
			t.Fatal("service initialization failed")
		}
	}
	select {
	case r := <-done:
		if r.err != nil || r.service == nil {
			t.Fatal("service startup failed")
		}
		t.Cleanup(func() { _ = r.service.Close() })
		return r.service
	case <-time.After(5 * time.Second):
		t.Fatal("service startup did not join initialization")
	}
	return nil
}

func startServiceBackup(t *testing.T, s *Service, repository uuid.UUID) *supervisedRun {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	op := &supervisedRun{ready: make(chan Access, 1), done: make(chan struct{}), release: make(chan struct{}), cancel: cancel}
	go func() {
		op.err = s.WithBackup(ctx, repository, uuid.Must(uuid.NewV7()), func(ctx context.Context, access Access) error {
			op.ready <- access
			select {
			case <-ctx.Done(): return ctx.Err()
			case <-op.release: return nil
			}
		})
		close(op.done)
	}()
	t.Cleanup(func() { cancel(); waitSupervised(t, op) })
	return op
}

func serviceTLSClient(t *testing.T, cert []byte) *http.Client {
	t.Helper()
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(cert) {
		t.Fatal("service CA fixture")
	}
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}
	t.Cleanup(transport.CloseIdleConnections)
	return &http.Client{Transport: transport, Timeout: 3*time.Second}
}

func TestServiceProtectedStartupMultipleOwnersOfflineTLSAndClose(t *testing.T) {
	f := newServiceFixture(t, 2, "success")
	s := startServiceFixture(t, f) // Center replay socket absent throughout backup.
	client := serviceTLSClient(t, f.certificate)
	url := "https://" + s.listener.Addr().String()
	one := startServiceBackup(t, s, f.config.Repositories[0].Binding.RepositoryID)
	two := startServiceBackup(t, s, f.config.Repositories[1].Binding.RepositoryID)
	a, b := waitAccess(t, one), waitAccess(t, two)
	for _, check := range []struct{ path string; access Access; status int }{
		{a.EndpointPath+"config", a, 200}, {b.EndpointPath+"config", b, 200},
		{b.EndpointPath+"config", a, 401}, {"/metrics", a, 403},
	} {
		req, _ := http.NewRequest(http.MethodGet, url+check.path, nil)
		req.SetBasicAuth(check.access.Username, string(check.access.Password))
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal("service TLS request failed")
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != check.status || resp.TLS == nil {
			t.Fatal("service route crossed repository identity or TLS boundary")
		}
	}
	finishSupervised(t, one)
	finishSupervised(t, two)
	second := startServiceBackup(t, s, f.config.Repositories[0].Binding.RepositoryID)
	_ = waitAccess(t, second)
	finishSupervised(t, second)
	if s.WithBackup(context.Background(), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), func(context.Context, Access) error { t.Error("unknown repo callback"); return nil }) != ErrService {
		t.Fatal("unconfigured repository admitted")
	}
	var closes sync.WaitGroup
	for range 4 {
		closes.Go(func() { if s.Close() != nil { t.Error("ordinary service shutdown failed") } })
	}
	closes.Wait()
	for _, r := range f.config.Repositories {
		assertSupervisorClean(t, f.state, f.config.RuntimeDirectory, BackupRequest{Binding: Binding{RepositoryID: r.Binding.RepositoryID}})
	}
	for _, r := range s.repositories {
		if len(bytes.Trim(r.source, "\x00")) != 0 || r.owner.raw != nil {
			t.Fatal("service retained private source or cloud plaintext after Close")
		}
		if _, err := os.Lstat(r.config.MaterialSocket); !os.IsNotExist(err) {
			t.Fatal("initialization socket survived cleanup")
		}
		if _, err := os.Lstat(r.config.AuthoritySocket); !os.IsNotExist(err) {
			t.Fatal("authorization socket survived cleanup")
		}
	}
	for i, r := range f.config.Repositories {
		q, err := gatewaypending.Recover(r.QueueDirectory, r.Binding, [32]byte(f.config.RecipientPublic), f.sources[i], f.central.Public().(ed25519.PublicKey), gatewaypending.Limits{MaxBytes: f.config.MaxBytes, MaxRecords: f.config.MaxRecords})
		if err != nil { t.Fatal("closed service lost durable repository evidence") }
		if wire, err := q.Next(); err != nil || wire == nil { t.Fatal("offline audit records missing") }
		_ = q.Close()
	}
	if restarted, err := StartService(context.Background(), f.config); err != ErrService || restarted != nil {
		t.Fatal("old queues/config revived a service owner")
	}
}

func TestServicePartialInitializationCancellationKeepsEncryptedEvidence(t *testing.T) {
	f := newServiceFixture(t, 2, "success")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := StartService(ctx, f.config); done <- err }()
	r := f.config.Repositories[0]
	waitServiceSocket(t, r.MaterialSocket)
	if gatewaypending.SendMaterial(ctx, r.MaterialSocket, uint32(os.Geteuid()), r.Binding, f.sources[0],
		func(_ context.Context, proof []byte) ([]byte, error) { return serviceMaterial(t, f, 0, proof), nil }, func(context.Context) error { return nil }) != nil {
		t.Fatal("first partial initialization failed")
	}
	waitServiceSocket(t, f.config.Repositories[1].MaterialSocket)
	cancel()
	select {
	case err := <-done: if err != ErrService { t.Fatal("partial canceled startup accepted") }
	case <-time.After(4*time.Second): t.Fatal("partial initialization did not join")
	}
	q, err := gatewaypending.Recover(r.QueueDirectory, r.Binding, [32]byte(f.config.RecipientPublic), f.sources[0], f.central.Public().(ed25519.PublicKey), gatewaypending.Limits{MaxBytes: f.config.MaxBytes, MaxRecords: f.config.MaxRecords})
	if err != nil { t.Fatal("partial startup discarded or retained repository lock") }
	_ = q.Close()
	global, err := gatewaypending.RecoverGlobalAudit(f.config.AuditQueueDirectory, f.config.AuditOrigin, [32]byte(f.config.RecipientPublic), f.auditSource, f.central.Public().(ed25519.PublicKey), gatewaypending.Limits{MaxBytes: f.config.MaxBytes, MaxRecords: f.config.MaxRecords})
	if err != nil { t.Fatal("partial startup discarded global audit evidence") }
	defer global.Close()
	wire, err := global.Next()
	if err != nil || wire == nil { t.Fatal("canceled startup lost independent rejection audit") }
	opened, err := security.OpenGatewayPending(wire, f.auditSource, f.pending)
	if err != nil || opened.Kind != "global_audit" || opened.Event.Reason != "material_rejected" { t.Fatal("partial failure guessed repository identity") }
}

func TestServiceAuthorityRevocationJoinsActiveSessionAndFreezesAllOwners(t *testing.T) {
	f := newServiceFixture(t, 2, "success")
	s := startServiceFixture(t, f)
	op := startServiceBackup(t, s, f.config.Repositories[0].Binding.RepositoryID)
	_ = waitAccess(t, op)
	r := f.config.Repositories[0]
	wire, err := security.SignGatewayStatement(security.GatewayStatement{Binding: r.Binding, Revision: 2, IssuedAt: time.Now().Unix(), Revoked: true}, f.central)
	if err != nil { t.Fatal(err) }
	if gatewaypending.SendAuthorization(context.Background(), r.AuthoritySocket, uint32(os.Geteuid()), r.Binding, f.sources[0],
		func(context.Context) ([]byte, error) { return wire, nil }, func(context.Context) error { return nil }) != nil { t.Fatal("service RFGA revocation failed") }
	select {
	case <-s.done: if s.Wait() != ErrService { t.Fatal("service revocation did not fail closed") }
	case <-time.After(5*time.Second): t.Fatal("service revocation did not join cleanup")
	}
	waitSupervised(t, op)
	if op.err != ErrService { t.Fatal("revoked callback survived") }
	assertSupervisorClean(t, f.state, f.config.RuntimeDirectory, BackupRequest{Binding: Binding{RepositoryID: r.Binding.RepositoryID}})
	for _, r := range s.repositories { if r.owner.raw != nil || len(bytes.Trim(r.source, "\x00")) != 0 { t.Fatal("revocation left another owner material live") } }
	if s.WithBackup(context.Background(), f.config.Repositories[1].Binding.RepositoryID, uuid.Must(uuid.NewV7()), func(context.Context, Access) error { return nil }) != ErrService { t.Fatal("failed service revived unrelated owner") }
}

func TestServiceReplayLostReceiptKeepsExactWireAndRecoversAfterOutage(t *testing.T) {
	f := newServiceFixture(t, 1, "success")
	s := startServiceFixture(t, f)
	if s.global.Record(context.Background(), Event{Action: "denied", Reason: "route_unavailable"}) != nil { t.Fatal("offline observation") }
	initial, err := s.globalQueue.Next()
	if err != nil || initial == nil { t.Fatal("offline wire missing") }
	l, err := gatewaypending.ListenReplay(f.config.ReplaySocket)
	if err != nil { t.Fatal(err) }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	var mu sync.Mutex
	var first []byte
	attempts := 0
	go func() {
		done <- gatewaypending.ServeReplay(ctx, l, uint32(os.Geteuid()), func(_ context.Context, runtime uuid.UUID, wire []byte) ([]byte, error) {
			mu.Lock(); defer mu.Unlock()
			if runtime != f.config.AuditOrigin.RuntimeID || !bytes.Equal(wire, initial) { t.Error("retry changed exact wire/runtime") }
			attempts++
			if attempts == 1 { first = bytes.Clone(wire); return nil, errors.New("private-lost-receipt") }
			r, err := security.OpenGatewayPending(wire, f.auditSource, f.pending)
			if err != nil { return nil, err }
			return security.SignGatewayPendingReceipt(security.GatewayPendingReceipt{AuditOriginID: r.Header.AuditOrigin.OriginID, RuntimeID: runtime,
				Sequence: r.Header.Sequence, RecordID: r.Header.RecordID, WireHash: security.GatewayPendingHash(wire)}, f.central)
		}, func(context.Context) error { return nil })
	}()
	defer func() { cancel(); if <-done != nil { t.Error("service replay fixture exit") } }()
	deadline := time.Now().Add(6*time.Second)
	for {
		wire, err := s.globalQueue.Next()
		mu.Lock(); accepted := attempts >= 2; mu.Unlock()
		if err == nil && wire == nil && accepted { break }
		if time.Now().After(deadline) { t.Fatal("service did not retry exact pending record") }
		time.Sleep(10*time.Millisecond)
	}
	mu.Lock(); retried := attempts >= 2 && bytes.Equal(first, initial); mu.Unlock()
	if !retried || !s.global.Ready() { t.Fatal("lost transport receipt revoked authority or replaced wire") }
}

func TestServicePartialInvalidMaterialNeverBindsPublicAndKeepsEvidence(t *testing.T) {
	f := newServiceFixture(t, 2, "success")
	reservation, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil { t.Fatal("public address fixture") }
	f.config.ListenAddress = reservation.Addr().String()
	_ = reservation.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := StartService(ctx, f.config); done <- err }()
	for i, r := range f.config.Repositories {
		waitServiceSocket(t, r.MaterialSocket)
		materialFixture := f
		if i == 1 {
			probe, err := net.Listen("tcp", f.config.ListenAddress)
			if err != nil { t.Fatal("partial startup exposed public ingress") }
			_ = probe.Close()
			materialFixture.config.RecipientPublic = bytes.Clone(f.config.RecipientPublic)
			materialFixture.config.RecipientPublic[0] ^= 1
		}
		err := gatewaypending.SendMaterial(ctx, r.MaterialSocket, uint32(os.Geteuid()), r.Binding, f.sources[i],
			func(_ context.Context, proof []byte) ([]byte, error) { return serviceMaterial(t, materialFixture, i, proof), nil }, func(context.Context) error { return nil })
		if (i == 0 && err != nil) || (i == 1 && err == nil) { t.Fatal("partial initialization accepted wrong recipient or rejected valid material") }
	}
	select {
	case err := <-done: if err != ErrService { t.Fatal("invalid partial startup accepted") }
	case <-time.After(4*time.Second): t.Fatal("invalid partial startup did not join")
	}
	r := f.config.Repositories[0]
	q, err := gatewaypending.Recover(r.QueueDirectory, r.Binding, [32]byte(f.config.RecipientPublic), f.sources[0], f.central.Public().(ed25519.PublicKey), gatewaypending.Limits{MaxBytes: f.config.MaxBytes, MaxRecords: f.config.MaxRecords})
	if err != nil { t.Fatal("partial failure lost first repository evidence") }
	_ = q.Close()
	for _, r := range f.config.Repositories {
		if _, err := os.Lstat(r.MaterialSocket); !os.IsNotExist(err) { t.Fatal("failed startup left material listener") }
		if _, err := os.Lstat(r.AuthoritySocket); !os.IsNotExist(err) { t.Fatal("failed startup opened authority listener") }
	}
}

func TestServiceGlobalFailureAndInvalidReceiptStopAllOwnersKeepWire(t *testing.T) {
	for _, failure := range []string{"global-capacity", "invalid-receipt"} {
		t.Run(failure, func(t *testing.T) {
			f := newServiceFixture(t, 2, "success")
			s := startServiceFixture(t, f)
			one := startServiceBackup(t, s, f.config.Repositories[0].Binding.RepositoryID)
			two := startServiceBackup(t, s, f.config.Repositories[1].Binding.RepositoryID)
			_ = waitAccess(t, one); _ = waitAccess(t, two)
			received := make(chan []byte, 1)
			if failure == "invalid-receipt" {
				l, err := gatewaypending.ListenReplay(f.config.ReplaySocket)
				if err != nil { t.Fatal("bad receipt listener") }
				ctx, cancel := context.WithCancel(context.Background())
				done := make(chan error, 1)
				go func() { done <- gatewaypending.ServeReplay(ctx, l, uint32(os.Geteuid()), func(_ context.Context, _ uuid.UUID, wire []byte) ([]byte, error) {
					if h, err := security.InspectGatewayPending(wire, f.auditSource); err == nil && h.AuditOrigin == f.config.AuditOrigin {
						received <- bytes.Clone(wire)
						return []byte("invalid-receipt-canary"), nil
					}
					for i, r := range f.config.Repositories {
						if h, err := security.InspectGatewayPending(wire, f.sources[i]); err == nil && h.Binding == r.Binding {
							return security.SignGatewayPendingReceipt(security.GatewayPendingReceipt{AdmissionID: h.Binding.AdmissionID, RuntimeID: h.Binding.RuntimeID,
								Sequence: h.Sequence, RecordID: h.RecordID, WireHash: security.GatewayPendingHash(wire)}, f.central)
						}
					}
					return nil, gatewaypending.ErrChannel
				}, func(context.Context) error { return nil }) }()
				defer func() { cancel(); if <-done != nil { t.Error("receipt listener cleanup") } }()
			} else {
				// Exhaust the genuine configured queue, never mutate its limits.
				for range f.config.MaxRecords - 1 {
					if s.global.Record(context.Background(), Event{Action: "denied", Reason: "route_unavailable"}) != nil { t.Fatal("global capacity fixture") }
				}
			}
			if s.global.Record(context.Background(), Event{Action: "denied", Reason: "route_unavailable"}) != nil { t.Fatal("global observation lost") }
			select {
			case <-s.done: if s.Wait() != ErrService { t.Fatal("global failure did not stop service") }
			case <-time.After(6*time.Second): t.Fatal("global failure did not join all owners")
			}
			waitSupervised(t, one); waitSupervised(t, two)
			if one.err != ErrService || two.err != ErrService { t.Fatal("global failure left active backup") }
			for _, r := range s.repositories {
				if r.owner.raw != nil || len(bytes.Trim(r.source, "\x00")) != 0 { t.Fatal("global failure retained source or material") }
				assertSupervisorClean(t, f.state, f.config.RuntimeDirectory, BackupRequest{Binding: Binding{RepositoryID: r.config.Binding.RepositoryID}})
			}
			q, err := gatewaypending.RecoverGlobalAudit(f.config.AuditQueueDirectory, f.config.AuditOrigin, [32]byte(f.config.RecipientPublic), f.auditSource, f.central.Public().(ed25519.PublicKey), gatewaypending.Limits{MaxBytes: f.config.MaxBytes, MaxRecords: f.config.MaxRecords})
			if err != nil { t.Fatal("global failure lost encrypted evidence") }
			defer q.Close()
			wire, err := q.Next()
			if err != nil || wire == nil { t.Fatal("unconfirmed global record was removed") }
			if failure == "invalid-receipt" && !bytes.Equal(wire, <-received) { t.Fatal("invalid receipt changed or removed original wire") }
			if s.WithBackup(context.Background(), f.config.Repositories[0].Binding.RepositoryID, uuid.Must(uuid.NewV7()), func(context.Context, Access) error { return nil }) != ErrService { t.Fatal("failed service revived owner") }
		})
	}
}
