package httpapi

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sagehou/restfleet/internal/domain"
	"github.com/sagehou/restfleet/internal/gateway"
	"github.com/sagehou/restfleet/internal/gatewaypending"
	"github.com/sagehou/restfleet/internal/security"
)

func globalAuditIntegrationFixture(t *testing.T, backend string) (pendingIntegration, domain.GatewayAuditBinding, *gatewaypending.Queue, *gateway.GlobalAuditRecorder) {
	t.Helper()
	f, startup := startupIntegrationFixture(t, backend) // No repository grant/source/material.
	b := domain.GatewayAuditBinding{OriginID: uuid.Must(uuid.NewV7()), RuntimeID: startup.Binding.RuntimeID}
	o, recipient, err := f.control.RegisterGatewayGlobalAudit(context.Background(), b, f.sourcePublic)
	if err != nil || o.Binding != b || !bytes.Equal(o.PublicKey, f.sourcePublic) {
		t.Fatal("global audit registration")
	}
	dir := t.TempDir()
	if os.Chmod(dir, 0700) != nil {
		t.Fatal("global queue permissions")
	}
	q, err := gatewaypending.CreateGlobalAudit(dir, b, recipient, f.source, f.centralPublic, gatewaypending.Limits{MaxBytes: 2 << 20, MaxRecords: 16})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { q.Close() })
	r, err := gateway.NewGlobalAuditRecorder(q, b, f.sourcePublic, f.centralPublic)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Close)
	return f, b, q, r
}

func globalAuditCounts(t *testing.T, f pendingIntegration, records int) {
	t.Helper()
	var origins, accepted int
	if err := f.pool.QueryRow(context.Background(), `select (select count(*) from gateway_audit_origins),(select count(*) from gateway_audit_records)`).Scan(&origins, &accepted); err != nil || origins != 1 || accepted != records {
		t.Fatalf("global effects origins=%d records=%d: %v", origins, accepted, err)
	}
	startupCounts(t, f, 0, 0, 0) // Audit-only ingestion cannot create backup authority.
}

func TestGlobalAuditOfflineSpoolRealReplayLostReceiptAndSeal(t *testing.T) {
	for _, backend := range []string{"onedrive", "drive", "webdav"} {
		t.Run(backend, func(t *testing.T) {
			f, b, q, r := globalAuditIntegrationFixture(t, backend)
			var beforeSecrets int
			if err := f.pool.QueryRow(context.Background(), "select count(*) from secrets").Scan(&beforeSecrets); err != nil {
				t.Fatal(err)
			}
			// No central call occurs while these observations are accepted.
			for _, event := range []gateway.Event{{Action: "denied", Reason: "route_unavailable"}, {Action: "denied", Reason: "rate_limited"}, {Action: "channel_denied", Reason: "authority_rejected"}} {
				if r.Record(context.Background(), event) != nil {
					t.Fatal("offline durable audit acceptance")
				}
			}
			globalAuditCounts(t, f, 0)
			dir, err := os.MkdirTemp("", "rfg-global-replay-")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(dir)
			path := filepath.Join(dir, "replay.sock")
			listener, err := gatewaypending.ListenReplay(path)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			var drop atomic.Bool
			drop.Store(true)
			go func() {
				done <- gatewaypending.ServeReplay(ctx, listener, uint32(os.Geteuid()), func(ctx context.Context, runtime uuid.UUID, wire []byte) ([]byte, error) {
					ack, err := f.control.ReplayGatewayRecord(ctx, runtime, wire)
					if err == nil && drop.CompareAndSwap(true, false) {
						return nil, errors.New("private-lost-receipt-canary")
					}
					return ack, err
				}, f.control.RecordGatewayReplayDenied)
			}()
			defer func() {
				cancel()
				if <-done != nil {
					t.Error("global replay listener exit")
				}
			}()
			wire, err := q.Next()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := gatewaypending.Replay(ctx, path, uint32(os.Geteuid()), b.RuntimeID, wire); err != gatewaypending.ErrChannel {
				t.Fatal("lost receipt fixture")
			}
			globalAuditCounts(t, f, 1)
			if pending, err := q.Next(); err != nil || !bytes.Equal(pending, wire) {
				t.Fatal("lost receipt reclaimed unconfirmed observation")
			}
			// Concurrent exact replay returns the same signed receipt and effect.
			var wg sync.WaitGroup
			var reference []byte
			var mu sync.Mutex
			for range 4 {
				wg.Go(func() {
					ack, err := f.control.ReplayGatewayRecord(ctx, b.RuntimeID, wire)
					mu.Lock()
					defer mu.Unlock()
					if err != nil {
						t.Error("concurrent exact replay")
						return
					}
					if reference == nil {
						reference = ack
					} else if !bytes.Equal(reference, ack) {
						t.Error("replay receipt changed")
					}
				})
			}
			wg.Wait()
			globalAuditCounts(t, f, 1)
			r.Close()
			if gatewaypending.Drain(ctx, q, path, uint32(os.Geteuid()), b.RuntimeID) != nil {
				t.Fatal("global drain")
			}
			globalAuditCounts(t, f, 3)
			seq, hash, err := q.Tail()
			if err != nil || seq != 3 {
				t.Fatal("global tail")
			}
			if f.control.SealGatewayGlobalAudit(ctx, b, seq, strings.Repeat("a", 64)) != domain.ErrGatewayGlobalAudit {
				t.Fatal("wrong tail sealed origin")
			}
			if f.control.SealGatewayGlobalAudit(ctx, b, seq, hash) != nil {
				t.Fatal("exact global seal")
			}
			if ack, err := f.control.ReplayGatewayRecord(ctx, b.RuntimeID, wire); err != nil || !bytes.Equal(ack, reference) {
				t.Fatal("sealed exact receipt replay")
			}
			if _, _, err := f.control.RegisterGatewayGlobalAudit(ctx, b, f.sourcePublic); err != domain.ErrGatewayGlobalAudit {
				t.Fatal("closed global origin revived")
			}
			var invalid, secrets int
			if err := f.pool.QueryRow(ctx, `select count(*) from audit_events a join gateway_audit_records r on a.id=r.record_id
				where a.resource_id is not null or a.actor_id is not null or a.resource_type<>'GATEWAY' or a.changes<>'{}'::jsonb`).Scan(&invalid); err != nil || invalid != 0 {
				t.Fatal("global audit guessed repository/request identity")
			}
			if err := f.pool.QueryRow(ctx, "select count(*) from secrets").Scan(&secrets); err != nil || secrets != beforeSecrets {
				t.Fatal("audit ingestion wrote cloud material")
			}
		})
	}
}

func TestGlobalAuditReplayRejectsSourceRuntimeOrderingSealAndAuditFailure(t *testing.T) {
	for _, failure := range []string{"source", "runtime", "unknown-origin", "future", "before-registration", "jump", "sealed", "audit", "canceled"} {
		t.Run(failure, func(t *testing.T) {
			f, b, q, r := globalAuditIntegrationFixture(t, "onedrive")
			if r.Record(context.Background(), gateway.Event{Action: "denied", Reason: "route_unavailable"}) != nil {
				t.Fatal("observation")
			}
			wire, err := q.Next()
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			runtime := b.RuntimeID
			h, err := security.InspectGatewayPending(wire, f.sourcePublic)
			if err != nil {
				t.Fatal(err)
			}
			record := security.GatewayPendingRecord{Header: h, Kind: "global_audit", Event: &domain.GatewayEvent{Action: "denied", Reason: "route_unavailable"}}
			source := f.source
			switch failure {
			case "source":
				_, source, err = ed25519.GenerateKey(rand.Reader)
				defer clear(source)
			case "runtime":
				runtime = uuid.Must(uuid.NewV7())
			case "unknown-origin":
				record.Header.AuditOrigin.OriginID = uuid.Must(uuid.NewV7())
			case "future":
				record.Header.CreatedAt = time.Now().Add(time.Minute).Unix()
			case "before-registration":
				record.Header.CreatedAt = time.Now().Add(-time.Minute).Unix()
			case "jump":
				record.Header.Sequence = 2
			case "sealed":
				err = f.control.SealGatewayGlobalAudit(ctx, b, 0, strings.Repeat("0", 64))
			case "audit":
				_, err = f.pool.Exec(ctx, `create function reject_global_audit() returns trigger language plpgsql as $$ begin
				if NEW.action='GATEWAY_REQUEST_DENIED' then raise exception 'private-global-audit-canary'; end if; return NEW; end $$;
				create trigger reject_global_audit before insert on audit_events for each row execute function reject_global_audit()`)
				t.Cleanup(func() {
					if _, err := f.pool.Exec(context.Background(), "drop trigger if exists reject_global_audit on audit_events;drop function if exists reject_global_audit()"); err != nil {
						t.Error("audit injection cleanup", err)
					}
				})
			case "canceled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			if err != nil {
				t.Fatal(err)
			}
			wire, err = security.SealGatewayPending(record, f.recipient, source)
			if err != nil {
				t.Fatal(err)
			}
			if ack, err := f.control.ReplayGatewayRecord(ctx, runtime, wire); err == nil || ack != nil || strings.Contains(err.Error(), "canary") {
				t.Fatal("forbidden global replay succeeded or leaked error")
			}
			globalAuditCounts(t, f, 0)
		})
	}
}

func TestGlobalOriginRegistrationCannotReplaceKeyRuntimeOrClosedSource(t *testing.T) {
	f, b, _, _ := globalAuditIntegrationFixture(t, "onedrive")
	ctx := context.Background()
	first, _, err := f.control.RegisterGatewayGlobalAudit(ctx, b, f.sourcePublic)
	if err != nil {
		t.Fatal("exact registration replay")
	}
	for _, change := range []struct {
		binding domain.GatewayAuditBinding
		source  ed25519.PublicKey
	}{
		{b, f.centralPublic},
		{domain.GatewayAuditBinding{OriginID: b.OriginID, RuntimeID: uuid.Must(uuid.NewV7())}, f.sourcePublic},
		{domain.GatewayAuditBinding{OriginID: uuid.Must(uuid.NewV7()), RuntimeID: b.RuntimeID}, f.sourcePublic},
		{b, make([]byte, 31)},
	} {
		if _, _, err := f.control.RegisterGatewayGlobalAudit(ctx, change.binding, change.source); err != domain.ErrGatewayGlobalAudit {
			t.Fatal("registration changed audit identity")
		}
	}
	if stored, err := f.store.GatewayAuditOrigin(ctx, b); err != nil || !stored.CreatedAt.Equal(first.CreatedAt) || !bytes.Equal(stored.PublicKey, f.sourcePublic) {
		t.Fatal("idempotent registration changed pin/time")
	}
	globalAuditCounts(t, f, 0)
}

func TestGlobalRegistrationAuditFailureAndConflictingReplayAreAtomic(t *testing.T) {
	t.Run("registration-audit", func(t *testing.T) {
		f, startup := startupIntegrationFixture(t, "onedrive")
		ctx := context.Background()
		if _, err := f.pool.Exec(ctx, `create function reject_global_registration() returns trigger language plpgsql as $$ begin
		 if NEW.action='GATEWAY_AUDIT_ORIGIN' then raise exception 'private-registration-canary'; end if; return NEW; end $$;
		 create trigger reject_global_registration before insert on audit_events for each row execute function reject_global_registration()`); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if _, err := f.pool.Exec(ctx, "drop trigger if exists reject_global_registration on audit_events;drop function if exists reject_global_registration()"); err != nil {
				t.Error("registration injection cleanup", err)
			}
		})
		b := domain.GatewayAuditBinding{OriginID: uuid.Must(uuid.NewV7()), RuntimeID: startup.Binding.RuntimeID}
		if _, _, err := f.control.RegisterGatewayGlobalAudit(ctx, b, f.sourcePublic); err != domain.ErrGatewayGlobalAudit {
			t.Fatal("failed registration returned a source")
		}
		var count int
		if err := f.pool.QueryRow(ctx, "select count(*) from gateway_audit_origins").Scan(&count); err != nil || count != 0 {
			t.Fatal("registration audit failure left partial origin")
		}
		startupCounts(t, f, 0, 0, 0)
	})
	t.Run("conflict", func(t *testing.T) {
		f, b, q, r := globalAuditIntegrationFixture(t, "drive")
		ctx := context.Background()
		if r.Record(ctx, gateway.Event{Action: "denied", Reason: "route_unavailable"}) != nil {
			t.Fatal("observation")
		}
		wire, err := q.Next()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.control.ReplayGatewayRecord(ctx, b.RuntimeID, wire); err != nil {
			t.Fatal(err)
		}
		h, err := security.InspectGatewayPending(wire, f.sourcePublic)
		if err != nil {
			t.Fatal(err)
		}
		changed, err := security.SealGatewayPending(security.GatewayPendingRecord{Header: h, Kind: "global_audit",
			Event: &domain.GatewayEvent{Action: "denied", Reason: "rate_limited"}}, f.recipient, f.source)
		if err != nil {
			t.Fatal(err)
		}
		if ack, err := f.control.ReplayGatewayRecord(ctx, b.RuntimeID, changed); err != domain.ErrGatewayGlobalAudit || ack != nil {
			t.Fatal("same record ID/sequence accepted different observation")
		}
		globalAuditCounts(t, f, 1)
	})
}
