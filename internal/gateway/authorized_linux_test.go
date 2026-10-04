package gateway

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sagehou/restfleet/internal/domain"
	"github.com/sagehou/restfleet/internal/gatewaypending"
	"github.com/sagehou/restfleet/internal/security"
)

func authorizedFixture(t *testing.T, mode string, capacity int, lifetime ...time.Duration) (*AuthorizedBackup, *Supervisor, *gatewaypending.Queue, security.GatewayStatement, ed25519.PrivateKey, ed25519.PublicKey, []byte, string, string) {
	t.Helper()
	q, a, statement, key, source, private := pendingGatewayFixture(t, gatewaypending.Limits{MaxBytes: 2 << 20, MaxRecords: capacity})
	s, state, root := supervisorFixture(t, mode, 2)
	if len(lifetime) == 1 {
		statement.Revision++
		statement.ExpiresAt = time.Now().Add(lifetime[0]).Unix()
		if a.Accept(statementWire(t, statement, key)) != nil {
			t.Fatal("fixture lifetime")
		}
	}
	owner, err := NewAuthorizedBackup(s, a, q, authorizedOrigin(statement, source), backupFixture().Config, "encrypted")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(owner.Close)
	return owner, s, q, statement, key, source, private, state, root
}

func authorizedOrigin(s security.GatewayStatement, source ed25519.PublicKey) domain.GatewayPendingOrigin {
	b := s.Binding
	return domain.GatewayPendingOrigin{Admission: domain.BackupAdmission{ID: b.AdmissionID, Owner: b.Owner,
		AgentID: b.AgentID, HostID: b.HostID, RepositoryID: b.RepositoryID, GatewayID: b.GatewayID,
		StorageCredentialID: b.StorageCredentialID, DeliveryID: b.DeliveryID, GatewaySecretRef: b.GatewaySecretRef,
		ResticSecretRef: b.ResticSecretRef, ConfigurationHash: b.ConfigurationHash,
		CreatedAt: time.Unix(s.IssuedAt-1, 0), ExpiresAt: time.Unix(s.ExpiresAt, 0)},
		RuntimeID: b.RuntimeID, PublicKey: source, InitialSecretRevision: 1}
}

func startAuthorized(t *testing.T, owner *AuthorizedBackup, run func(context.Context, Access) error) *supervisedRun {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	op := &supervisedRun{ready: make(chan Access, 1), done: make(chan struct{}), release: make(chan struct{}), cancel: cancel}
	go func() {
		op.err = owner.WithBackup(ctx, uuid.Must(uuid.NewV7()), func(ctx context.Context, access Access) error {
			op.ready <- access
			if run != nil {
				return run(ctx, access)
			}
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

func drainAuthorized(t *testing.T, q *gatewaypending.Queue, key ed25519.PrivateKey, source ed25519.PublicKey, private []byte) []security.GatewayPendingRecord {
	t.Helper()
	var records []security.GatewayPendingRecord
	for {
		wire, err := q.Next()
		if err != nil {
			t.Fatal(err)
		}
		if wire == nil {
			return records
		}
		r, err := security.OpenGatewayPending(wire, source, private)
		if err != nil {
			t.Fatal(err)
		}
		records = append(records, r)
		h := r.Header
		ack, err := security.SignGatewayPendingReceipt(security.GatewayPendingReceipt{AdmissionID: h.Binding.AdmissionID,
			RuntimeID: h.Binding.RuntimeID, Sequence: h.Sequence, RecordID: h.RecordID, WireHash: security.GatewayPendingHash(wire)}, key)
		if err != nil || q.Acknowledge(ack) != nil {
			t.Fatal("exact acknowledgment failed")
		}
	}
}

func TestAuthorizedBackupSequentialSessionsKeepRefreshedMaterial(t *testing.T) {
	owner, supervisor, queue, _, key, source, private, state, root := authorizedFixture(t, "refresh", 32)
	// Every scoped event must use its own durable recorder, even with no central
	// audit service. There is no DB port anywhere in this local owner.
	supervisor.audit = func(context.Context, Event) error { return errors.New("center unavailable") }
	op := startAuthorized(t, owner, nil)
	access := waitAccess(t, op)
	if supervisorRequest(supervisor, access, "config").Code != http.StatusOK {
		t.Fatal("accepted local grant did not authorize data plane")
	}
	if os.WriteFile(filepath.Join(state, "update-"+owner.authorization.binding.RepositoryID.String()), []byte("1"), 0600) != nil {
		t.Fatal("refresh trigger")
	}
	for deadline := time.Now().Add(3 * time.Second); ; {
		owner.material.Lock()
		refreshed := bytes.Contains(owner.raw, []byte("refreshed-token"))
		owner.material.Unlock()
		if refreshed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("watcher did not update durable local material")
		}
		time.Sleep(10 * time.Millisecond)
	}
	old := bytes.Clone(access.Password)
	finishSupervised(t, op)
	assertSupervisorClean(t, state, root, backupFixture())
	if os.Remove(filepath.Join(state, "update-"+owner.authorization.binding.RepositoryID.String())) != nil {
		t.Fatal("reset fixture trigger")
	}
	// A subsequent backup keeps token/revision and gets a fresh session secret.
	op = startAuthorized(t, owner, nil)
	next := waitAccess(t, op)
	if bytes.Equal(next.Password, old) {
		t.Fatal("session capability reused")
	}
	stale := next
	stale.Password = old
	if supervisorRequest(supervisor, stale, "config").Code != http.StatusUnauthorized {
		t.Fatal("previous capability accepted by new session")
	}
	if os.WriteFile(filepath.Join(state, "update-"+owner.authorization.binding.RepositoryID.String()), []byte("second"), 0600) != nil {
		t.Fatal("second refresh trigger")
	}
	for deadline := time.Now().Add(3 * time.Second); ; {
		owner.material.Lock()
		refreshed := bytes.Contains(owner.raw, []byte("refreshed-token-second"))
		owner.material.Unlock()
		if refreshed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("second watcher did not retain and advance material")
		}
		time.Sleep(10 * time.Millisecond)
	}
	finishSupervised(t, op)
	owner.Close()
	records := drainAuthorized(t, queue, key, source, private)
	refreshes := 0
	for _, r := range records {
		if r.Kind == "refresh" {
			refreshes++
			if r.ExpectedSecretRevision != int64(refreshes) || !bytes.Contains(r.Config, []byte("refreshed-token")) ||
				(refreshes == 2 && !bytes.Contains(r.Config, []byte("refreshed-token-second"))) {
				t.Fatal("refresh version or config changed")
			}
		}
	}
	if refreshes != 2 || len(records) != 7 { // two start/end, two refreshes, stale auth rejection
		t.Fatalf("unexpected durable history: records=%d refreshes=%d", len(records), refreshes)
	}
	if _, _, err := queue.Tail(); err != nil || len(owner.raw) != 0 {
		t.Fatal("owner did not freeze and clear after joined cleanup")
	}
}

func TestAuthorizedBackupConcurrentOriginsStayIsolated(t *testing.T) {
	first, supervisor, firstQueue, _, firstKey, firstSource, firstPrivate, _, _ := authorizedFixture(t, "success", 16)
	route := Binding{HostID: uuid.Must(uuid.NewV7()), RepositoryID: uuid.Must(uuid.NewV7()), GatewayID: uuid.Must(uuid.NewV7()), OperationID: uuid.Must(uuid.NewV7())}
	q, a, statement, key, source, private := pendingGatewayFixture(t, gatewaypending.Limits{MaxBytes: 2 << 20, MaxRecords: 16}, route)
	second, err := NewAuthorizedBackup(supervisor, a, q, authorizedOrigin(statement, source), backupFixture().Config, "encrypted")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(second.Close)
	supervisor.audit = func(context.Context, Event) error { return ErrGatewayAudit }
	one, two := startAuthorized(t, first, nil), startAuthorized(t, second, nil)
	oneAccess, twoAccess := waitAccess(t, one), waitAccess(t, two)
	if supervisorRequest(supervisor, oneAccess, "config").Code != http.StatusOK || supervisorRequest(supervisor, twoAccess, "config").Code != http.StatusOK {
		t.Fatal("independent local sessions unavailable")
	}
	foreign := twoAccess
	foreign.Username, foreign.Password = oneAccess.Username, bytes.Clone(oneAccess.Password)
	if supervisorRequest(supervisor, foreign, "config").Code != http.StatusUnauthorized {
		t.Fatal("cross-Host capability accepted")
	}
	finishSupervised(t, one)
	finishSupervised(t, two)
	first.Close()
	second.Close()
	for _, check := range []struct {
		records []security.GatewayPendingRecord
		host    uuid.UUID
		count   int
	}{
		{drainAuthorized(t, firstQueue, firstKey, firstSource, firstPrivate), first.authorization.binding.HostID, 2},
		{drainAuthorized(t, q, key, source, private), route.HostID, 3},
	} {
		if len(check.records) != check.count {
			t.Fatal("scoped event reached the wrong source")
		}
		for _, record := range check.records {
			if record.Header.Binding.HostID != check.host || record.Event == nil || record.Event.Binding.HostID != check.host {
				t.Fatal("cross-Host audit binding")
			}
		}
	}
}

func TestAuthorizedBackupRevocationExpiryClockAndCapacityJoin(t *testing.T) {
	for _, failure := range []string{"revoked", "expired", "clock", "capacity", "frozen", "closed"} {
		t.Run(failure, func(t *testing.T) {
			capacity := 8
			if failure == "capacity" {
				capacity = 2
			}
			owner, supervisor, queue, statement, key, _, _, state, root := authorizedFixture(t, "success", capacity)
			op := startAuthorized(t, owner, nil)
			access := waitAccess(t, op)
			// Borrowed password is deliberately copied so cleanup cannot make a
			// negative request pass simply by zeroing the authentication material.
			access.Password = bytes.Clone(access.Password)
			switch failure {
			case "revoked":
				statement.Revision++
				statement.Revoked, statement.ExpiresAt = true, 0
				if owner.authorization.Accept(statementWire(t, statement, key)) != nil {
					t.Fatal("revocation delivery")
				}
			case "expired":
				if owner.authorization.statusAt(time.Unix(statement.ExpiresAt, 0)) != AuthorizationExpired {
					t.Fatal("expiry")
				}
			case "clock":
				if owner.authorization.statusAt(time.Now().Add(-time.Minute)) != AuthorizationClockUnsafe {
					t.Fatal("clock failure")
				}
			case "capacity":
				if owner.record(context.Background(), Event{Action: "denied", Reason: "route_unavailable"}) != nil {
					t.Fatal("fill queue")
				}
			case "frozen":
				queue.Freeze()
			case "closed":
				owner.Close()
			}
			// Idle work must stop without a request triggering cancellation.
			waitSupervised(t, op)
			if supervisorRequest(supervisor, access, "config").Code == http.StatusOK {
				t.Fatal("invalid local authority forwarded request")
			}
			if op.err == nil {
				t.Fatal("unsafe run reported success")
			}
			assertSupervisorClean(t, state, root, backupFixture())
			if owner.WithBackup(context.Background(), uuid.Must(uuid.NewV7()), func(context.Context, Access) error { return nil }) != ErrAuthorization {
				t.Fatal("failed owner restarted")
			}
			// No draining, re-admission or central release is attempted here.
		})
	}
}

func TestAuthorizedBackupRejectsForeignRegistrationAndSecondOwner(t *testing.T) {
	q, a, statement, key, source, _ := pendingGatewayFixture(t, gatewaypending.Limits{MaxBytes: 2 << 20, MaxRecords: 16})
	supervisor, _, _ := supervisorFixture(t, "success", 1)
	origin := authorizedOrigin(statement, source)
	for _, change := range []func(*domain.GatewayPendingOrigin){
		func(o *domain.GatewayPendingOrigin) { o.Admission.Owner = uuid.Must(uuid.NewV7()) },
		func(o *domain.GatewayPendingOrigin) { o.Admission.HostID = uuid.Must(uuid.NewV7()) },
		func(o *domain.GatewayPendingOrigin) { o.Admission.ConfigurationHash = "foreign" },
		func(o *domain.GatewayPendingOrigin) { o.RuntimeID = uuid.Must(uuid.NewV7()) },
		func(o *domain.GatewayPendingOrigin) { o.PublicKey = key.Public().(ed25519.PublicKey) },
		func(o *domain.GatewayPendingOrigin) { o.ClosedAt = new(time.Now()) },
		func(o *domain.GatewayPendingOrigin) { o.Admission.ReleasedAt = new(time.Now()) },
		func(o *domain.GatewayPendingOrigin) { o.Admission.ExpiresAt = time.Now().Add(-time.Second) },
		func(o *domain.GatewayPendingOrigin) { o.Admission.ExpiresAt = time.Unix(statement.ExpiresAt-1, 0) },
		func(o *domain.GatewayPendingOrigin) { o.InitialSecretRevision = 0 },
	} {
		bad := origin
		change(&bad)
		if _, err := NewAuthorizedBackup(supervisor, a, q, bad, backupFixture().Config, "encrypted"); err != ErrAuthorization {
			t.Fatal("foreign registration authorized data plane")
		}
	}
	owner, err := NewAuthorizedBackup(supervisor, a, q, origin, backupFixture().Config, "encrypted")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(owner.Close)
	if _, err := NewAuthorizedBackup(supervisor, a, q, origin, backupFixture().Config, "encrypted"); err != ErrAuthorization {
		t.Fatal("second owner reset token/source state")
	}
	owner.Close()
	if _, err := NewAuthorizedBackup(supervisor, a, q, origin, backupFixture().Config, "encrypted"); err != ErrAuthorization {
		t.Fatal("closed source was reused")
	}
}

func TestLocalAuthorityRecheckedBetweenProbeAndWrite(t *testing.T) {
	var permitted atomic.Bool
	permitted.Store(true)
	backend := &fakeBackend{objects: map[string]string{}}
	socket := socketServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backend.ServeHTTP(w, r)
		if r.Method == http.MethodHead {
			permitted.Store(false)
		}
	}))
	session, secret := newSession(t, socket, bindingFixture(), acceptAudit)
	session.guard = permitted.Load
	body := "new object"
	serve(t, session, request(session, secret, http.MethodPost, object("data", body), body), http.StatusBadGateway)
	state := backend.inspect()
	if len(state.calls) != 1 || len(state.objects) != 0 {
		t.Fatal("lost authority between probe and write still created an object")
	}
}

func TestAuthorizedFailureCannotRestartAfterSuccessfulDrain(t *testing.T) {
	owner, _, queue, _, key, source, private, _, _ := authorizedFixture(t, "success", 16)
	op := startAuthorized(t, owner, func(context.Context, Access) error { return errors.New("callback-secret-canary") })
	waitSupervised(t, op)
	if op.err != ErrBackupFailed {
		t.Fatal("callback failure was not sanitized")
	}
	drainAuthorized(t, queue, key, source, private)
	if owner.authorization.Status() != AuthorizationValid || len(owner.raw) != 0 || queue.CheckProducer(owner.authorization.binding, source, owner.authorization.key) != gatewaypending.ErrQueue {
		t.Fatal("failed owner retained plaintext or allowed append under a valid grant")
	}
	if owner.WithBackup(context.Background(), uuid.Must(uuid.NewV7()), func(context.Context, Access) error { return nil }) != ErrAuthorization {
		t.Fatal("drain resurrected failed owner")
	}
}

func TestAuthorizedIdleOwnerClearsMaterialWithoutStartingBackup(t *testing.T) {
	for _, failure := range []string{"revoked", "expired", "admission_expired", "clock", "capacity", "frozen", "queue_closed", "owner_closed"} {
		t.Run(failure, func(t *testing.T) {
			capacity := 16
			if failure == "capacity" {
				capacity = 1
			}
			var lifetime []time.Duration
			if failure == "admission_expired" {
				lifetime = []time.Duration{2 * time.Second}
			}
			owner, supervisor, queue, statement, key, source, private, state, root := authorizedFixture(t, "success", capacity, lifetime...)
			// Borrow the existing buffer: nil alone would not prove secret erasure.
			owner.material.Lock()
			borrowed := owner.raw
			owner.material.Unlock()
			if len(borrowed) == 0 {
				t.Fatal("missing idle material")
			}
			switch failure {
			case "revoked":
				statement.Revision++
				statement.Revoked, statement.ExpiresAt = true, 0
				if owner.authorization.Accept(statementWire(t, statement, key)) != nil {
					t.Fatal("idle revocation")
				}
			case "expired":
				// Let actual wall/monotonic time expire a short signed grant; no
				// request or explicit Status call is permitted to trigger cleanup.
				statement.Revision++
				statement.ExpiresAt = time.Now().Unix() + 1
				if owner.authorization.Accept(statementWire(t, statement, key)) != nil {
					t.Fatal("short idle grant")
				}
			case "admission_expired":
				statement.Revision++
				statement.ExpiresAt = time.Now().Add(time.Minute).Unix()
				if owner.authorization.Accept(statementWire(t, statement, key)) != nil {
					t.Fatal("longer grant must not extend the original admission")
				}
			case "clock":
				if owner.authorization.statusAt(time.Now().Add(-time.Minute)) != AuthorizationClockUnsafe {
					t.Fatal("idle clock failure")
				}
			case "capacity":
				if owner.record(context.Background(), Event{Action: "denied", Reason: "route_unavailable"}) != nil {
					t.Fatal("fill idle queue")
				}
			case "frozen":
				queue.Freeze()
			case "queue_closed":
				if queue.Close() != nil {
					t.Fatal("close idle queue")
				}
			case "owner_closed":
				var callers sync.WaitGroup
				for range 4 {
					callers.Go(owner.Close)
				}
				callers.Wait()
			}
			select {
			case <-owner.watchDone:
			case <-time.After(5 * time.Second):
				t.Fatal("idle owner retained material until a backup request")
			}
			if len(owner.raw) != 0 || len(bytes.Trim(borrowed, "\x00")) != 0 ||
				queue.CheckProducer(statement.Binding, source, owner.authorization.key) != gatewaypending.ErrQueue {
				t.Fatal("idle cleanup did not erase/freeze runtime material")
			}
			assertSupervisorClean(t, state, root, backupFixture())
			if failure == "capacity" {
				if len(drainAuthorized(t, queue, key, source, private)) != 1 {
					t.Fatal("idle cleanup discarded durable evidence")
				}
			}
			// A higher signed grant or a successful drain cannot revive this
			// failed owner. A revocation remains terminal in Authorization too.
			if failure != "revoked" && failure != "clock" {
				statement.Revision++
				statement.ExpiresAt = time.Now().Add(time.Minute).Unix()
				if owner.authorization.Accept(statementWire(t, statement, key)) != nil {
					t.Fatal("renewal fixture")
				}
			}
			if owner.WithBackup(context.Background(), uuid.Must(uuid.NewV7()), func(context.Context, Access) error { return nil }) != ErrAuthorization {
				t.Fatal("failed idle owner revived")
			}
		})
	}
}
