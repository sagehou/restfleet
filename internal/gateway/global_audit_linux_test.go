package gateway

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sagehou/restfleet/internal/domain"
	"github.com/sagehou/restfleet/internal/gatewaypending"
	"github.com/sagehou/restfleet/internal/security"
	"golang.org/x/crypto/nacl/box"
)

func globalRecorderFixture(t *testing.T, capacity int) (*GlobalAuditRecorder, *gatewaypending.Queue, ed25519.PublicKey, ed25519.PrivateKey, []byte) {
	t.Helper()
	source, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(key)
	_, central, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { clear(central) })
	recipient, private, err := box.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { clear(private[:]) })
	dir := t.TempDir()
	if os.Chmod(dir, 0700) != nil {
		t.Fatal("private queue")
	}
	b := security.GatewayAuditBinding{OriginID: uuid.Must(uuid.NewV7()), RuntimeID: uuid.Must(uuid.NewV7())}
	q, err := gatewaypending.CreateGlobalAudit(dir, b, *recipient, key, central.Public().(ed25519.PublicKey), gatewaypending.Limits{MaxBytes: 2 << 20, MaxRecords: capacity})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { q.Close() })
	r, err := NewGlobalAuditRecorder(q, b, source, central.Public().(ed25519.PublicKey))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Close)
	return r, q, source, central, private[:]
}

func TestGlobalRecorderKeepsOnlyFixedUnboundEventsAndSanitizesInvalid(t *testing.T) {
	r, q, source, central, private := globalRecorderFixture(t, 8)
	for _, event := range []Event{
		{Action: "denied", Reason: "route_unavailable"},
		{Action: "denied", Reason: "rate_limited"},
		{Action: "channel_denied", Reason: "material_rejected"},
		{Action: "channel_denied", Reason: "authority_rejected"},
	} {
		if r.Record(context.Background(), event) != nil {
			t.Fatal("global observation failed")
		}
	}
	// A route identity can never be smuggled into this process-only source.
	if r.Record(context.Background(), Event{Binding: bindingFixture(), Action: "session_start", Reason: "requested"}) != ErrGatewayAudit || r.Ready() {
		t.Fatal("scoped event accepted or failed recorder revived")
	}
	count := 0
	for {
		wire, err := q.Next()
		if err != nil {
			t.Fatal(err)
		}
		if wire == nil {
			break
		}
		opened, openErr := security.OpenGatewayPending(wire, source, private)
		if openErr != nil || opened.Event.Binding != (domain.GatewayBinding{}) || opened.Config != nil || opened.Header.AuthorizationRevision != 0 || opened.Header.Binding != (security.GatewayAuthorizationBinding{}) {
			t.Fatal("global event carried repository authority or material")
		}
		count++
		if count == 5 && (opened.Event.Action != "event_rejected" || opened.Event.Reason != "invalid_event") {
			t.Fatal("invalid event was not sanitized")
		}
		h := opened.Header
		ack, err := security.SignGatewayPendingReceipt(security.GatewayPendingReceipt{AuditOriginID: r.binding.OriginID, RuntimeID: r.binding.RuntimeID,
			Sequence: h.Sequence, RecordID: h.RecordID, WireHash: security.GatewayPendingHash(wire)}, central)
		if err != nil || q.Acknowledge(ack) != nil {
			t.Fatal("global acknowledgment")
		}
	}
	if count != 5 {
		t.Fatal("missing global observations")
	}
}

func TestGlobalAuditCapacityClockAndShutdownCancelActiveOwner(t *testing.T) {
	for _, failure := range []string{"capacity", "clock", "closed", "queue-closed"} {
		t.Run(failure, func(t *testing.T) {
			r, q, source, central, private := globalRecorderFixture(t, 1)
			ownedQueue, authorization, statement, key, ownerSource, _ := pendingGatewayFixture(t, gatewaypending.Limits{MaxBytes: 2 << 20, MaxRecords: 16})
			supervisor, state, root := supervisorFixture(t, "success", 1)
			// Set the trusted guard before starting any session. Production
			// supplies the same pair through NewSupervisor's constructor.
			supervisor.audit, supervisor.auditReady = r.Record, r.Ready
			owner, err := NewAuthorizedBackup(supervisor, authorization, ownedQueue, authorizedOrigin(statement, ownerSource), backupFixture().Config, "encrypted")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(owner.Close)
			owner.material.Lock()
			borrowed := owner.raw
			owner.material.Unlock()
			op := startAuthorized(t, owner, nil)
			_ = waitAccess(t, op)
			switch failure {
			case "capacity":
				if r.Record(context.Background(), Event{Action: "denied", Reason: "route_unavailable"}) != nil {
					t.Fatal("fill global queue")
				}
			case "clock":
				r.mu.Lock()
				r.clock = func() time.Time { return time.Now().Add(-time.Minute) }
				r.mu.Unlock()
			case "closed":
				r.Close()
			case "queue-closed":
				q.Close()
			}
			waitSupervised(t, op)
			if op.err == nil {
				t.Fatal("active owner ignored global audit failure")
			}
			if r.Ready() || len(owner.raw) != 0 || len(bytes.Trim(borrowed, "\x00")) != 0 {
				t.Fatal("global failure retained authorized plaintext")
			}
			assertSupervisorClean(t, state, root, backupFixture())
			if failure == "capacity" {
				wire, err := q.Next()
				opened, openErr := security.OpenGatewayPending(wire, source, private)
				if err != nil || openErr != nil || opened.Header.AuditOrigin != r.binding {
					t.Fatal("pending global observation lost")
				}
				h := opened.Header
				ack, err := security.SignGatewayPendingReceipt(security.GatewayPendingReceipt{AuditOriginID: r.binding.OriginID, RuntimeID: r.binding.RuntimeID,
					Sequence: h.Sequence, RecordID: h.RecordID, WireHash: security.GatewayPendingHash(wire)}, central)
				if err != nil || q.Acknowledge(ack) != nil || r.Ready() {
					t.Fatal("drain revived failed global recorder")
				}
			}
			statement.Revision++
			if owner.AcceptAuthorization(context.Background(), statementWire(t, statement, key)) != ErrAuthorization {
				t.Fatal("renewal revived owner after global audit failure")
			}
		})
	}
}
