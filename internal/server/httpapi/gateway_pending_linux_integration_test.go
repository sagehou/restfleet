package httpapi

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sagehou/restfleet/internal/domain"
	"github.com/sagehou/restfleet/internal/gateway"
	"github.com/sagehou/restfleet/internal/gatewaypending"
	"github.com/sagehou/restfleet/internal/persistence/postgres"
	"github.com/sagehou/restfleet/internal/rclone"
	"github.com/sagehou/restfleet/internal/security"
	control "github.com/sagehou/restfleet/internal/server"
	"golang.org/x/crypto/nacl/box"
)

type pendingIntegration struct {
	store                       *postgres.Store
	pool                        *pgxpool.Pool
	control                     *control.ControlPlane
	request                     domain.GatewayDecisionRequest
	statement                   security.GatewayStatement
	centralPublic, sourcePublic ed25519.PublicKey
	source                      ed25519.PrivateKey
	recipient                   [32]byte
	raw                         []byte
	remote                      string
	initialRevision             int64
	queue                       *gatewaypending.Queue
	authorization               *gateway.Authorization
	browser                     *testBrowser
}

func pendingIntegrationFixture(t *testing.T, backend string) pendingIntegration {
	t.Helper()
	s, pool, b, enrolled, repo, previous := deliveryFixture(t, backend)
	initializeDeliveryRepository(t, s, b, repo)
	a := acceptedAdmissionRequest(t, pool, previous, enrolled.AgentId)
	if _, err := s.ReserveBackupAdmission(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	public, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	recipient, private, err := box.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	c, err := control.NewControlPlane(s, control.Settings{MasterKey: bytes.Repeat([]byte{8}, 32), GatewayPublicURL: "https://gateway.example", GatewaySigningKey: key, GatewayPendingDecryptionKey: private[:], Enrollment: control.EnrollmentSettings{ServerCABundlePEM: []byte(enrolled.CaBundlePem)}, ExpectedSchema: postgres.ExpectedSchemaVersion, PasswordParams: security.Argon2Params{Memory: 64, Iterations: 1, Parallelism: 1, SaltLength: 8, KeyLength: 16}})
	clear(key)
	clear(private[:])
	if err != nil {
		t.Fatal(err)
	}
	r := domain.GatewayDecisionRequest{ID: uuid.Must(uuid.NewV7()), AdmissionID: a.ID, Owner: a.Owner, RuntimeID: uuid.Must(uuid.NewV7()), Lifetime: 10 * time.Minute}
	wire, err := c.DecideGatewayAuthorization(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	statement, err := security.VerifyGatewayStatement(wire, public)
	if err != nil {
		t.Fatal(err)
	}
	sourcePublic, source, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	origin, recipientPublic, err := c.RegisterGatewayPending(context.Background(), statement.Binding, sourcePublic)
	if err != nil || recipientPublic != *recipient || origin.InitialSecretRevision != 1 {
		t.Fatal("origin registration failed")
	}
	authorization, err := gateway.NewAuthorization(public, statement.Binding)
	if err != nil || authorization.Accept(wire) != nil {
		t.Fatal("authorization")
	}
	path := t.TempDir()
	if os.Chmod(path, 0700) != nil {
		t.Fatal("queue directory")
	}
	q, err := gatewaypending.Create(path, statement.Binding, recipientPublic, source, public, gatewaypending.Limits{MaxBytes: 4 << 20, MaxRecords: 16})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = q.Close(); clear(source) })
	f := pendingIntegration{s, pool, c, r, statement, public, sourcePublic, source, recipientPublic, nil, "", origin.InitialSecretRevision, q, authorization, b}
	err = c.WithBackupMaterial(context.Background(), a.ID, a.Owner, func(_ context.Context, _ domain.BackupAdmission, raw []byte, remote string, _ func(context.Context, []byte) error) error {
		f.raw = append([]byte(nil), raw...)
		f.remote = remote
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { clear(f.raw) })
	return f
}

func (f pendingIntegration) record(kind string) security.GatewayPendingRecord {
	r := security.GatewayPendingRecord{Header: security.GatewayPendingHeader{Binding: f.statement.Binding, RecordID: uuid.Must(uuid.NewV7()), Sequence: 1, PreviousHash: strings.Repeat("0", 64), AuthorizationRevision: 1, CreatedAt: time.Now().Unix()}, Kind: kind}
	if kind == "audit" {
		r.Event = &domain.GatewayEvent{Binding: domain.GatewayBinding{HostID: f.statement.Binding.HostID, RepositoryID: f.statement.Binding.RepositoryID, GatewayID: f.statement.Binding.GatewayID, OperationID: uuid.Must(uuid.NewV7())}, Action: "session_start", Reason: "requested"}
	}
	if kind == "refresh" {
		r.ExpectedSecretRevision = f.initialRevision
		r.Config = changedPendingToken(f.raw, "pending-refreshed-canary")
	}
	return r
}

// Fixtures contain canonical OAuth JSON. Keep token replacement independent of
// the production parser so the test can also exercise central validation.
func changedPendingToken(raw []byte, value string) []byte {
	start := bytes.Index(raw, []byte(`"access_token":"`))
	if start < 0 {
		return nil
	}
	start += len(`"access_token":"`)
	end := bytes.IndexByte(raw[start:], '"') + start
	return append(append(append([]byte(nil), raw[:start]...), value...), raw[end:]...)
}

func (f pendingIntegration) seal(t *testing.T, r security.GatewayPendingRecord) []byte {
	t.Helper()
	wire, err := security.SealGatewayPending(r, f.recipient, f.source)
	if err != nil {
		t.Fatal(err)
	}
	return wire
}

func (f pendingIntegration) count(t *testing.T, records, audits int, revision int64) {
	t.Helper()
	var n, a int
	var rev int64
	err := f.pool.QueryRow(context.Background(), `select (select count(*) from gateway_pending_records),
		(select count(*) from audit_events where action in ('GATEWAY_SESSION_START','GATEWAY_SESSION_END','GATEWAY_LOCK_CLEANUP_INTENT','GATEWAY_PENDING_REFRESH')),
		(select secret_revision from storage_credentials where id=$1)`, f.statement.Binding.StorageCredentialID).Scan(&n, &a, &rev)
	if err != nil || n != records || a != audits || rev != revision {
		t.Fatalf("effects: records=%d audits=%d revision=%d error=%v", n, a, rev, err)
	}
}

func pendingReplayListener(t *testing.T, c *control.ControlPlane, dropFirst bool) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "rfg-central-")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "replay.sock")
	l, err := gatewaypending.ListenReplay(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	var dropped atomic.Bool
	go func() {
		done <- gatewaypending.ServeReplay(ctx, l, uint32(os.Geteuid()), func(ctx context.Context, id uuid.UUID, wire []byte) ([]byte, error) {
			ack, err := c.ReplayGatewayPending(ctx, id, wire)
			if err == nil && dropFirst && dropped.CompareAndSwap(false, true) {
				return nil, errors.New("test-only-confirmation-lost")
			}
			return ack, err
		}, c.RecordGatewayReplayDenied)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(6 * time.Second):
			t.Error("replay listener did not join")
		}
		_ = os.RemoveAll(dir)
	})
	return path
}

func TestGatewayPendingProviderPipelineAndLostCommitAcknowledgement(t *testing.T) {
	for _, backend := range []string{"onedrive", "drive", "webdav"} {
		t.Run(backend, func(t *testing.T) {
			f := pendingIntegrationFixture(t, backend)
			ctx := context.Background()
			record, err := gateway.NewPendingAuditRecorder(f.queue, f.authorization)
			if err != nil {
				t.Fatal(err)
			}
			event := *f.record("audit").Event
			if record(ctx, event) != nil {
				t.Fatal("audit not durable")
			}
			persist, closeRecorder, err := gateway.NewPendingRefreshRecorder(f.queue, f.authorization, f.raw, f.remote, f.initialRevision)
			if err != nil {
				t.Fatal(err)
			}
			defer closeRecorder()
			if persist(ctx, f.raw) != nil {
				t.Fatal("identical config created an effect")
			}
			want, revision := 3, int64(1)
			if backend != "webdav" {
				next := changedPendingToken(f.raw, "pending-refreshed-canary")
				defer clear(next)
				if persist(ctx, next) != nil {
					t.Fatal("refresh not durable")
				}
				want++
				revision++
			} else {
				changed := bytes.Replace(f.raw, []byte("webdav-bearer-canary"), []byte("changed-bearer-canary"), 1)
				if persist(ctx, changed) != rclone.ErrConfigChanged {
					t.Fatal("WebDAV credential accepted as refresh")
				}
			}
			event.Action = "lock_cleanup"
			event.Reason = "owned_lock"
			event.Authenticated = true
			if record(ctx, event) != nil {
				t.Fatal("intent audit")
			}
			event.Action = "session_end"
			event.Reason = "finished"
			event.Authenticated = false
			if record(ctx, event) != nil {
				t.Fatal("cleanup observation")
			}
			closeRecorder()
			f.queue.Freeze()
			// No local control listener: pending acceptance does not touch PostgreSQL.
			if gatewaypending.Drain(ctx, f.queue, "/tmp/restfleet-no-such-replay/socket", uint32(os.Geteuid()), f.request.RuntimeID) == nil {
				t.Fatal("absent control channel accepted")
			}
			f.count(t, 0, 0, 1)
			first, _ := f.queue.Next()
			path := pendingReplayListener(t, f.control, true)
			if gatewaypending.Drain(ctx, f.queue, path, uint32(os.Geteuid()), f.request.RuntimeID) == nil {
				t.Fatal("lost ACK reported as complete")
			}
			f.count(t, 1, 1, 1)
			again, _ := f.queue.Next()
			if !bytes.Equal(first, again) {
				t.Fatal("commit ACK loss lost/re-encrypted pending data")
			}
			if gatewaypending.Drain(ctx, f.queue, path, uint32(os.Geteuid()), f.request.RuntimeID) != nil {
				t.Fatal("idempotent replay failed")
			}
			f.count(t, want, want, revision)
			if err := f.store.VerifyAuditChain(ctx); err != nil {
				t.Fatal(err)
			}
			if backend != "webdav" {
				err = f.control.WithBackupMaterial(ctx, f.request.AdmissionID, f.request.Owner, func(_ context.Context, _ domain.BackupAdmission, raw []byte, _ string, _ func(context.Context, []byte) error) error {
					if !bytes.Contains(raw, []byte("pending-refreshed-canary")) {
						t.Error("encrypted refresh not recoverable")
					}
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			var cleartext bool
			var status string
			if err = f.pool.QueryRow(ctx, `select exists(select 1 from secrets where encode(ciphertext,'escape') like '%pending-refreshed-canary%'),(select status from repositories where id=$1)`, f.statement.Binding.RepositoryID).Scan(&cleartext, &status); err != nil || cleartext || status != "PROVISIONING" {
				t.Fatal("plaintext leaked or replay falsely marked READY")
			}
			revoke := f.request
			revoke.ID = uuid.Must(uuid.NewV7())
			revoke.ExpectedRevision = 1
			revoke.Revoke = true
			revoke.Lifetime = 0
			if _, err = f.control.DecideGatewayAuthorization(ctx, revoke); err != nil {
				t.Fatal(err)
			}
			if err = f.store.ReleaseBackupAdmission(ctx, f.request.AdmissionID, f.request.Owner); err != domain.ErrGatewayPending {
				t.Fatal("drain alone released fence")
			}
			sequence, hash, err := f.queue.Tail()
			if err != nil {
				t.Fatal(err)
			}
			if err = f.control.SealGatewayPending(ctx, f.request.AdmissionID, f.request.Owner, f.request.RuntimeID, sequence+1, hash); err != domain.ErrGatewayPending {
				t.Fatal("unreceived tail sealed")
			}
			if err = f.control.SealGatewayPending(ctx, f.request.AdmissionID, f.request.Owner, f.request.RuntimeID, sequence, hash); err != nil {
				t.Fatal(err)
			}
			// This fixture never starts a data plane. In production a trusted
			// coordinator must independently join all work before this release.
			if err = f.store.ReleaseBackupAdmission(ctx, f.request.AdmissionID, f.request.Owner); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestGatewayPendingRejectsBindingsOrderConflictsAndConcurrentDuplicates(t *testing.T) {
	f := pendingIntegrationFixture(t, "onedrive")
	ctx := context.Background()
	original := f.record("audit")
	wire := f.seal(t, original)
	for _, mutate := range []func(*security.GatewayPendingRecord){
		func(r *security.GatewayPendingRecord) { r.Header.Binding.RuntimeID = uuid.Must(uuid.NewV7()) },
		func(r *security.GatewayPendingRecord) { r.Header.Binding.Owner = uuid.Must(uuid.NewV7()) },
		func(r *security.GatewayPendingRecord) { r.Header.Binding.AgentID = uuid.Must(uuid.NewV7()) },
		func(r *security.GatewayPendingRecord) { r.Header.Sequence = 2 },
		func(r *security.GatewayPendingRecord) { r.Header.AuthorizationRevision = 2 },
		func(r *security.GatewayPendingRecord) { r.Header.CreatedAt = time.Now().Add(time.Hour).Unix() },
	} {
		bad := original
		mutate(&bad)
		if ack, err := f.control.ReplayGatewayPending(ctx, f.request.RuntimeID, f.seal(t, bad)); err != domain.ErrGatewayPending || ack != nil {
			t.Fatal("invalid binding/order accepted")
		}
	}
	var rejected int
	if err := f.pool.QueryRow(ctx, `select count(*) from audit_events where action='GATEWAY_PENDING_REPLAY_DENIED' and result='DENIED' and resource_id is null and actor_id is null and changes='{}'::jsonb`).Scan(&rejected); err != nil || rejected < 6 {
		t.Fatal("replay authorization failures lacked safe audits")
	}

	f.count(t, 0, 0, 1)
	var wg sync.WaitGroup
	acks := make(chan []byte, 8)
	for range 8 {
		wg.Go(func() {
			ack, err := f.control.ReplayGatewayPending(ctx, f.request.RuntimeID, wire)
			if err != nil {
				t.Error(err)
			}
			acks <- ack
		})
	}
	wg.Wait()
	close(acks)
	var first []byte
	for ack := range acks {
		if first == nil {
			first = ack
		}
		if !bytes.Equal(first, ack) {
			t.Fatal("duplicate commit ACK changed")
		}
	}
	f.count(t, 1, 1, 1)
	// Same plaintext, sequence and ID with fresh random encryption is a conflict.
	if ack, err := f.control.ReplayGatewayPending(ctx, f.request.RuntimeID, f.seal(t, original)); err != domain.ErrGatewayPending || ack != nil {
		t.Fatal("conflicting replay accepted")
	}
	next := original
	next.Header.Sequence = 2
	next.Header.PreviousHash = security.GatewayPendingHash(wire)
	if ack, err := f.control.ReplayGatewayPending(ctx, f.request.RuntimeID, f.seal(t, next)); err != domain.ErrGatewayPending || ack != nil {
		t.Fatal("record ID reused at another sequence")
	}
	next.Header.RecordID = uuid.Must(uuid.NewV7())
	next.Header.PreviousHash = strings.Repeat("b", 64)
	if ack, err := f.control.ReplayGatewayPending(ctx, f.request.RuntimeID, f.seal(t, next)); err != domain.ErrGatewayPending || ack != nil {
		t.Fatal("broken predecessor accepted")
	}
	f.count(t, 1, 1, 1)
	other, _, _ := ed25519.GenerateKey(rand.Reader)
	if _, _, err := f.control.RegisterGatewayPending(ctx, f.statement.Binding, other); err != domain.ErrGatewayPending {
		t.Fatal("origin rekeyed")
	}
	if ack, err := f.control.ReplayGatewayPending(ctx, uuid.Must(uuid.NewV7()), wire); err != domain.ErrGatewayPending || ack != nil {
		t.Fatal("self-selected origin accepted")
	}
}

func TestGatewayPendingCentralTokenOnlyAndCASFailuresRetainFence(t *testing.T) {
	for _, backend := range []string{"onedrive", "drive", "webdav"} {
		t.Run(backend, func(t *testing.T) {
			f := pendingIntegrationFixture(t, backend)
			ctx := context.Background()
			base := f.record("refresh")
			if backend == "webdav" {
				base.Config = bytes.Replace(f.raw, []byte("webdav-bearer-canary"), []byte("changed-bearer-canary"), 1)
			}
			for _, mode := range []string{"target", "crypt", "cas", "unchanged"} {
				r := base
				switch mode {
				case "target":
					r.Config = bytes.Replace(base.Config, []byte("cloud:backups"), []byte("cloud:other"), 1)
				case "crypt":
					r.Config = bytes.Replace(base.Config, []byte("password = "), []byte("password = Z"), 1)
				case "cas":
					r.ExpectedSecretRevision = 2
				case "unchanged":
					r.Config = f.raw
				}
				if ack, err := f.control.ReplayGatewayPending(ctx, f.request.RuntimeID, f.seal(t, r)); err != domain.ErrGatewayPending || ack != nil || strings.Contains(err.Error(), "canary") {
					t.Fatal("non-token/CAS change accepted or leaked")
				}
			}
			f.count(t, 0, 0, 1)
			var released *time.Time
			if err := f.pool.QueryRow(ctx, "select released_at from gateway_backup_admissions where id=$1", f.request.AdmissionID).Scan(&released); err != nil || released != nil {
				t.Fatal("failure released fence")
			}
		})
	}
}

func TestGatewayPendingAuditFailureRollsBackRefreshBeforeACK(t *testing.T) {
	for _, kind := range []string{"audit", "refresh"} {
		t.Run(kind, func(t *testing.T) {
			f := pendingIntegrationFixture(t, "onedrive")
			ctx := context.Background()
			wire := f.seal(t, f.record(kind))
			_, err := f.pool.Exec(ctx, `create function fail_pending_audit() returns trigger language plpgsql as $$ begin
			 if new.action in ('GATEWAY_SESSION_START','GATEWAY_PENDING_REFRESH') then raise exception 'audit-secret-canary'; end if; return new; end $$;
			 create trigger fail_pending_audit before insert on audit_events for each row execute function fail_pending_audit();`)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				_, _ = f.pool.Exec(ctx, "drop trigger if exists fail_pending_audit on audit_events;drop function if exists fail_pending_audit()")
			})
			if ack, err := f.control.ReplayGatewayPending(ctx, f.request.RuntimeID, wire); err != domain.ErrGatewayPending || ack != nil || strings.Contains(err.Error(), "canary") {
				t.Fatal("failed transaction acknowledged or leaked")
			}
			f.count(t, 0, 0, 1)
			var revisions, orphans int
			if err = f.pool.QueryRow(ctx, `select (select count(*) from storage_credential_revisions),(select count(*) from secrets s where kind='RCLONE_CONFIG' and not exists(select 1 from storage_credential_revisions r where r.secret_ref=s.id))`).Scan(&revisions, &orphans); err != nil || revisions != 1 || orphans != 0 {
				t.Fatal("rollback left encrypted partial effect")
			}
			if _, err = f.pool.Exec(ctx, "drop trigger fail_pending_audit on audit_events;drop function fail_pending_audit()"); err != nil {
				t.Fatal(err)
			}
			if ack, err := f.control.ReplayGatewayPending(ctx, f.request.RuntimeID, wire); err != nil || len(ack) == 0 {
				t.Fatal("retry did not commit")
			}
		})
	}
}

func TestGatewayPendingLateReplayAfterExpiryDisableAndRevocation(t *testing.T) {
	f := pendingIntegrationFixture(t, "onedrive")
	ctx := context.Background()
	r := f.record("refresh")
	// Test-only simulated historical timeline, not AGT-005 real-time evidence.
	// Grant issued -15m; record produced -10m; grant expired -5m; replay now.
	now := time.Now().UTC().Truncate(time.Second)

	_, err := f.pool.Exec(ctx, "update gateway_backup_admissions set created_at=$2,expires_at=$3 where id=$1", f.request.AdmissionID, now.Add(-time.Hour), now.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.pool.Exec(ctx, "update gateway_authorization_decisions set issued_at=$2,expires_at=$3 where admission_id=$1", f.request.AdmissionID, now.Add(-15*time.Minute), now.Add(-5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err = f.pool.Exec(ctx, "update agents set status='REVOKED';update storage_credentials set status='DISABLED'"); err != nil {
		t.Fatal(err)
	}

	r.Header.CreatedAt = now.Add(-10 * time.Minute).Unix()
	wire := f.seal(t, r)
	if ack, err := f.control.ReplayGatewayPending(ctx, f.request.RuntimeID, wire); err != nil || ack == nil {
		t.Fatal("late valid refresh discarded")
	}
	f.count(t, 1, 1, 2)
	if err = f.store.ReleaseBackupAdmission(ctx, f.request.AdmissionID, f.request.Owner); err != domain.ErrGatewayPending {
		t.Fatal("expiry released open source fence")
	}
	late := r
	late.Header.RecordID = uuid.Must(uuid.NewV7())
	late.Header.Sequence = 2
	late.Header.PreviousHash = security.GatewayPendingHash(wire)
	late.Header.CreatedAt = now.Unix()
	late.ExpectedSecretRevision = 2
	if ack, err := f.control.ReplayGatewayPending(ctx, f.request.RuntimeID, f.seal(t, late)); err != domain.ErrGatewayPending || ack != nil {
		t.Fatal("refresh produced after expiry accepted")
	}
	// Explicit central revocation stops new access, while old committed replay
	// remains idempotent and is not mistaken for a new grant or cleanup proof.
	revoke := f.request
	revoke.ID = uuid.Must(uuid.NewV7())
	revoke.ExpectedRevision = 1
	revoke.Revoke = true
	revoke.Lifetime = 0
	if _, err = f.control.DecideGatewayAuthorization(ctx, revoke); err != nil {
		t.Fatal(err)
	}
	if ack, err := f.control.ReplayGatewayPending(ctx, f.request.RuntimeID, wire); err != nil || ack == nil {
		t.Fatal("committed historical record lost ACK")
	}
	f.count(t, 1, 1, 2)
}

func TestGatewayPendingOneProcessMultipleAdmissionsRemainIsolated(t *testing.T) {
	f := pendingIntegrationFixture(t, "onedrive")
	ctx := context.Background()
	credential := createBackendCredential(t, f.browser, "drive")
	host := createTestHost(t, f.browser, "Second pending Host")
	repo := createTestRepository(t, f.browser, host.Id, credential.Id, "Second pending Repository")
	token := createTestEnrollmentToken(t, f.browser, host.Id)
	csr, key := agentCSR(t)
	clear(key)
	response := f.browser.request(t, http.MethodPost, "/api/v1/agent-enrollment", enrollRequest(token.Token, csr, uuid.Must(uuid.NewV7())), nil)
	if response.Code != http.StatusCreated {
		t.Fatal("second enrollment")
	}
	var enrolled AgentEnrollmentResponse
	decodeResponse(t, response, &enrolled)
	initializeDeliveryRepository(t, f.store, f.browser, repo)
	admission := acceptedAdmissionRequest(t, f.pool, f.control, enrolled.AgentId)
	if _, err := f.store.ReserveBackupAdmission(ctx, admission); err != nil {
		t.Fatal(err)
	}
	request := domain.GatewayDecisionRequest{ID: uuid.Must(uuid.NewV7()), AdmissionID: admission.ID, Owner: admission.Owner, RuntimeID: f.request.RuntimeID, Lifetime: 10 * time.Minute}
	statementWire, err := f.control.DecideGatewayAuthorization(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	statement, err := security.VerifyGatewayStatement(statementWire, f.centralPublic)
	if err != nil {
		t.Fatal(err)
	}
	public, source, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(source)
	if _, _, err = f.control.RegisterGatewayPending(ctx, statement.Binding, public); err != nil {
		t.Fatal("same process cannot register second admission")
	}
	first := f.record("audit")
	firstWire := f.seal(t, first)
	second := f.record("audit")
	second.Header.Binding = statement.Binding
	second.Event.Binding.HostID = statement.Binding.HostID
	second.Event.Binding.RepositoryID = statement.Binding.RepositoryID
	second.Event.Binding.GatewayID = statement.Binding.GatewayID
	wrongSource := f.seal(t, second)
	if ack, err := f.control.ReplayGatewayPending(ctx, request.RuntimeID, wrongSource); err != domain.ErrGatewayPending || ack != nil {
		t.Fatal("registry selector accepted another admission's source")
	}
	secondWire, err := security.SealGatewayPending(second, f.recipient, source)
	if err != nil {
		t.Fatal(err)
	}
	for _, wire := range [][]byte{firstWire, secondWire, firstWire, secondWire} {
		if ack, err := f.control.ReplayGatewayPending(ctx, request.RuntimeID, wire); err != nil || ack == nil {
			t.Fatal("independent sequence/replay failed")
		}
	}
	f.count(t, 2, 2, 1)
	if err := f.control.SealGatewayPending(ctx, f.request.AdmissionID, f.request.Owner, request.RuntimeID, 1, security.GatewayPendingHash(firstWire)); err != nil {
		t.Fatal(err)
	}
	var closed *time.Time
	if err := f.pool.QueryRow(ctx, "select closed_at from gateway_pending_origins where admission_id=$1", admission.ID).Scan(&closed); err != nil || closed != nil {
		t.Fatal("sealing one admission closed another")
	}
}

func TestGatewayPendingBlocksSynchronousRefreshBypass(t *testing.T) {
	f := pendingIntegrationFixture(t, "onedrive")
	ctx := context.Background()
	err := f.control.WithBackupMaterial(ctx, f.request.AdmissionID, f.request.Owner, func(ctx context.Context, _ domain.BackupAdmission, raw []byte, _ string, persist func(context.Context, []byte) error) error {
		next := changedPendingToken(raw, "bypass-canary")
		defer clear(next)
		if persist(ctx, next) != rclone.ErrRefreshPersist {
			t.Fatal("registered origin lost sole refresh ownership")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	f.count(t, 0, 0, 1)
}
