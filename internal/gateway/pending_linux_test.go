package gateway

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sagehou/restfleet/internal/gatewaypending"
	"github.com/sagehou/restfleet/internal/rclone"
	"github.com/sagehou/restfleet/internal/security"
	"golang.org/x/crypto/nacl/box"
)

func pendingGatewayFixture(t *testing.T, limits gatewaypending.Limits, routes ...Binding) (*gatewaypending.Queue, *Authorization, security.GatewayStatement, ed25519.PrivateKey, ed25519.PublicKey, []byte) {
	t.Helper()
	public, central, _ := ed25519.GenerateKey(rand.Reader)
	sourcePublic, source, _ := ed25519.GenerateKey(rand.Reader)
	recipient, private, _ := box.GenerateKey(rand.Reader)
	id := uuid.Must(uuid.NewV7())
	route := bindingFixture()
	if len(routes) != 0 {
		route = routes[0]
	}
	b := security.GatewayAuthorizationBinding{AdmissionID: id, Owner: id, RuntimeID: id, AgentID: id, HostID: route.HostID, RepositoryID: route.RepositoryID, GatewayID: route.GatewayID, StorageCredentialID: id, DeliveryID: id, GatewaySecretRef: id, ResticSecretRef: id, ConfigurationHash: strings.Repeat("a", 64)}
	a, err := NewAuthorization(public, b)
	if err != nil {
		t.Fatal(err)
	}
	s := security.GatewayStatement{Binding: b, Revision: 1, IssuedAt: time.Now().Add(-time.Second).Unix(), ExpiresAt: time.Now().Add(time.Hour).Unix()}
	if a.Accept(statementWire(t, s, central)) != nil {
		t.Fatal("grant")
	}
	path := t.TempDir()
	if os.Chmod(path, 0700) != nil {
		t.Fatal("directory")
	}
	q, err := gatewaypending.Create(path, b, *recipient, source, public, limits)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = q.Close(); clear(source); clear(central); clear(private[:]) })
	return q, a, s, central, sourcePublic, private[:]
}

func TestPendingRecordersRejectUnknownAndClosedCallbacks(t *testing.T) {
	q, a, s, key, source, private := pendingGatewayFixture(t, gatewaypending.Limits{MaxBytes: 2 << 20, MaxRecords: 8})
	ctx := context.Background()
	record, err := NewPendingAuditRecorder(q, a)
	if err != nil {
		t.Fatal(err)
	}
	if record(ctx, Event{Binding: bindingFixture(), Action: "secret-canary", Reason: "secret-canary"}) != ErrGatewayAudit {
		t.Fatal("invalid audit accepted")
	}
	wire, err := q.Next()
	if err != nil {
		t.Fatal(err)
	}
	r, err := security.OpenGatewayPending(wire, source, private)
	if err != nil || r.Event.Binding != (Binding{}) || r.Event.Action != "event_rejected" || r.Event.Reason != "invalid_event" {
		t.Fatal("invalid input was not sanitized before durable record")
	}
	raw := backupFixture().Config
	persist, closeRecorder, err := NewPendingRefreshRecorder(q, a, raw, "encrypted", 1)
	if err != nil {
		t.Fatal(err)
	}
	if persist(ctx, raw) != nil {
		t.Fatal("unchanged")
	}
	next := bytes.ReplaceAll(raw, []byte("fixture-refresh"), []byte("pending-refresh"))
	if persist(ctx, next) != nil {
		t.Fatal("token refresh")
	}
	changed := bytes.ReplaceAll(next, []byte("cloud:backups"), []byte("cloud:other"))
	if persist(ctx, changed) != rclone.ErrConfigChanged {
		t.Fatal("target change")
	}
	s.Revision = 2
	s.Revoked = true
	s.ExpiresAt = 0
	if a.Accept(statementWire(t, s, key)) != nil {
		t.Fatal("revocation")
	}
	if persist(ctx, next) != rclone.ErrRefreshPersist {
		t.Fatal("revoked refresh")
	}
	// Cleanup observations still reference the last non-revoked grant and do
	// not authorize any new data-plane action.
	if record(ctx, Event{Binding: bindingFixture(), Action: "session_end", Reason: "finished"}) != nil {
		t.Fatal("cleanup observation dropped")
	}
	closeRecorder()
	if persist(ctx, next) != rclone.ErrRefreshPersist {
		t.Fatal("retained callback")
	}
	q.Freeze()
	if record(ctx, Event{Binding: bindingFixture(), Action: "session_start", Reason: "requested"}) != ErrGatewayAudit {
		t.Fatal("frozen queue accepted")
	}
}

func TestSupervisorUsesDurablePendingAuditAndWatcher(t *testing.T) {
	for _, capacity := range []int{8, 1} {
		t.Run(fmt.Sprint(capacity), func(t *testing.T) {
			q, a, _, key, source, private := pendingGatewayFixture(t, gatewaypending.Limits{MaxBytes: 2 << 20, MaxRecords: capacity})
			record, err := NewPendingAuditRecorder(q, a)
			if err != nil {
				t.Fatal(err)
			}
			req := backupFixture()
			persist, closeRecorder, err := NewPendingRefreshRecorder(q, a, req.Config, req.Remote, 1)
			if err != nil {
				t.Fatal(err)
			}
			defer closeRecorder()
			supervisor, state, _ := supervisorFixture(t, "refresh", 1)
			supervisor.audit = record
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			observed := make(chan error, 1)
			watcher := func(ctx context.Context, raw []byte) error {
				err := persist(ctx, raw)
				select {
				case observed <- err:
				default:
				}
				return err
			}
			err = supervisor.WithBackup(ctx, req, watcher, func(ctx context.Context, _ Access) error {
				if os.WriteFile(filepath.Join(state, "update-"+req.Binding.RepositoryID.String()), []byte("1"), 0600) != nil {
					return errors.New("test trigger")
				}
				select {
				case err := <-observed:
					return err
				case <-ctx.Done():
					return ctx.Err()
				}
			})
			if capacity == 1 {
				if err == nil {
					t.Fatal("full queue did not cancel runtime")
				}
				wire, err := q.Next()
				if err != nil || wire == nil {
					t.Fatal("full queue lost unacknowledged start")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			q.Freeze()
			for seq := int64(1); seq <= 3; seq++ {
				wire, err := q.Next()
				if err != nil || wire == nil {
					t.Fatal("missing durable effect")
				}
				r, err := security.OpenGatewayPending(wire, source, private)
				if err != nil || r.Header.Sequence != seq {
					t.Fatal("invalid durable chain")
				}
				if seq == 2 && (r.Kind != "refresh" || !bytes.Contains(r.Config, []byte("refreshed-token"))) {
					t.Fatal("watcher did not persist encrypted refresh")
				}
				ack, err := security.SignGatewayPendingReceipt(security.GatewayPendingReceipt{AdmissionID: r.Header.Binding.AdmissionID, RuntimeID: r.Header.Binding.RuntimeID, Sequence: seq, RecordID: r.Header.RecordID, WireHash: security.GatewayPendingHash(wire)}, key)
				if err != nil || q.Acknowledge(ack) != nil {
					t.Fatal("ACK")
				}
			}
			if seq, _, err := q.Tail(); err != nil || seq != 3 {
				t.Fatal("extra/missing effects")
			}
		})
	}
}
