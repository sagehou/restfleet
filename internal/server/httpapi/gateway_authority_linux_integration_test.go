package httpapi

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"io"
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

func TestCentralAuthorityDeliveryRenewsReplaysAndRevokesDisabledIdentity(t *testing.T) {
	for _, backend := range []string{"onedrive", "drive", "webdav"} {
		t.Run(backend, func(t *testing.T) {
			f, startup := startupIntegrationFixture(t, backend)
			if sent, received := startupExchange(t, f, startup, func(_ context.Context, m security.GatewayMaterial) (func(), error) {
				var err error
				f.authorization, err = gateway.NewAuthorization(f.centralPublic, startup.Binding)
				if err != nil || f.authorization.Accept(m.Statement) != nil {
					return nil, security.ErrGatewayMaterial
				}
				return func() {}, nil
			}); sent != nil || received != nil {
				t.Fatal("initial material delivery failed")
			}
			dir, err := os.MkdirTemp("", "rfg-authority-")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(dir)
			path := filepath.Join(dir, "authority.sock")
			l, err := gatewaypending.ListenReplay(path)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			var accepted [][]byte
			go func() {
				done <- gatewaypending.ServeAuthorization(ctx, l, uint32(os.Geteuid()), startup.Binding, f.source, f.centralPublic,
					func(_ context.Context, wire []byte) error {
						if f.authorization.Accept(wire) != nil {
							return gateway.ErrAuthorization
						}
						accepted = append(accepted, bytes.Clone(wire))
						return nil
					}, f.control.RecordGatewayAuthorityDenied)
			}()
			var joinOnce sync.Once
			join := func() {
				joinOnce.Do(func() {
					cancel()
					if <-done != nil {
						t.Error("authority server did not join")
					}
				})
			}
			defer join()
			r := domain.GatewayDecisionRequest{ID: uuid.Must(uuid.NewV7()), AdmissionID: startup.Binding.AdmissionID, Owner: startup.Binding.Owner,
				RuntimeID: startup.Binding.RuntimeID, ExpectedRevision: 1, Lifetime: 5 * time.Minute}
			for range 2 {
				if f.control.DeliverGatewayAuthorization(ctx, path, uint32(os.Geteuid()), startup.Binding, f.sourcePublic, r) != nil {
					t.Fatal("renewal/exact replay failed")
				}
			}
			if _, err := f.pool.Exec(ctx, "update storage_credentials set status='DISABLED' where id=$1", startup.Binding.StorageCredentialID); err != nil {
				t.Fatal(err)
			}
			r.ID, r.ExpectedRevision, r.Revoke, r.Lifetime = uuid.Must(uuid.NewV7()), 2, true, 0
			if f.control.DeliverGatewayAuthorization(ctx, path, uint32(os.Geteuid()), startup.Binding, f.sourcePublic, r) != nil || f.authorization.Status() != gateway.AuthorizationRevoked {
				t.Fatal("disabled credential prevented explicit revocation delivery")
			}
			r.ID, r.ExpectedRevision, r.Revoke, r.Lifetime = uuid.Must(uuid.NewV7()), 3, false, time.Minute
			if f.control.DeliverGatewayAuthorization(ctx, path, uint32(os.Geteuid()), startup.Binding, f.sourcePublic, r) != security.ErrGatewayAuthority {
				t.Fatal("revoked binding renewed")
			}
			// Join before inspecting callback-owned slices, including race runs.
			join()
			if len(accepted) != 3 || !bytes.Equal(accepted[0], accepted[1]) {
				t.Fatal("ACK replay changed a committed grant/deadline")
			}
			startupCounts(t, f, 3, 1, 1)
		})
	}
}

func TestCentralAuthorityDeliveryRejectsBeforeNewDecisionOrMaterial(t *testing.T) {
	for _, failure := range []string{"source", "stored-source", "binding", "uid", "revision", "ack", "disabled", "sealed", "audit", "canceled"} {
		t.Run(failure, func(t *testing.T) {
			f, startup := startupIntegrationFixture(t, "onedrive")
			if sent, received := startupExchange(t, f, startup, func(context.Context, security.GatewayMaterial) (func(), error) {
				return func() {}, nil
			}); sent != nil || received != nil {
				t.Fatal("startup fixture")
			}
			dir, err := os.MkdirTemp("", "rfg-authority-negative-")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(dir)
			path := filepath.Join(dir, "authority.sock")
			l, err := gatewaypending.ListenReplay(path)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			binding, source, pin, uid := startup.Binding, f.source, f.sourcePublic, uint32(os.Geteuid())
			r := domain.GatewayDecisionRequest{ID: uuid.Must(uuid.NewV7()), AdmissionID: binding.AdmissionID, Owner: binding.Owner,
				RuntimeID: binding.RuntimeID, ExpectedRevision: 1, Lifetime: time.Minute}
			switch failure {
			case "source":
				pin = f.centralPublic
			case "stored-source":
				pin, source, err = ed25519.GenerateKey(rand.Reader)
				defer clear(source)
			case "binding":
				binding.HostID = uuid.Must(uuid.NewV7())
			case "uid":
				uid++
			case "revision":
				r.ExpectedRevision = 2
			case "ack":
				_, err = f.pool.Exec(ctx, "update repository_agent_deliveries set accepted_at=null where id=$1", binding.DeliveryID)
			case "disabled":
				_, err = f.pool.Exec(ctx, "update storage_credentials set status='DISABLED' where id=$1", binding.StorageCredentialID)
			case "sealed":
				err = f.control.SealGatewayPending(ctx, binding.AdmissionID, binding.Owner, binding.RuntimeID, 0, strings.Repeat("0", 64))
			case "audit":
				_, err = f.pool.Exec(ctx, `create function reject_authority_audit() returns trigger language plpgsql as $$ begin
				if NEW.action='GATEWAY_AUTHORIZATION_DECISION' then raise exception 'private-authority-canary'; end if; return NEW; end $$;
				create trigger reject_authority_audit before insert on audit_events for each row execute function reject_authority_audit()`)
				t.Cleanup(func() {
					if _, err := f.pool.Exec(context.Background(), "drop trigger if exists reject_authority_audit on audit_events;drop function if exists reject_authority_audit()"); err != nil {
						t.Error("audit injection cleanup", err)
					}
				})
			}
			if err != nil {
				cancel()
				l.Close()
				t.Fatal(err)
			}
			var applied atomic.Int32
			done := make(chan error, 1)
			go func() {
				done <- gatewaypending.ServeAuthorization(ctx, l, uint32(os.Geteuid()), binding, source, f.centralPublic,
					func(context.Context, []byte) error { applied.Add(1); return nil }, f.control.RecordGatewayAuthorityDenied)
			}()
			request := ctx
			if failure == "canceled" {
				var stop context.CancelFunc
				request, stop = context.WithCancel(ctx)
				stop()
			}
			err = f.control.DeliverGatewayAuthorization(request, path, uid, binding, pin, r)
			cancel()
			if err != security.ErrGatewayAuthority || <-done != nil || applied.Load() != 0 {
				t.Fatal("rejected authority reached Gateway or leaked raw error")
			}
			startupCounts(t, f, 1, 1, 1)
		})
	}
}

func TestCentralAuthorityLostReceiptReplaysExactCommittedGrant(t *testing.T) {
	f, startup := startupIntegrationFixture(t, "drive")
	if sent, received := startupExchange(t, f, startup, func(_ context.Context, m security.GatewayMaterial) (func(), error) {
		var err error
		f.authorization, err = gateway.NewAuthorization(f.centralPublic, startup.Binding)
		if err != nil || f.authorization.Accept(m.Statement) != nil {
			return nil, security.ErrGatewayMaterial
		}
		return func() {}, nil
	}); sent != nil || received != nil {
		t.Fatal("startup fixture")
	}
	dir, err := os.MkdirTemp("", "rfg-authority-receipt-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "authority.sock")
	l, err := gatewaypending.ListenReplay(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	var accepted [][]byte
	go func() {
		for attempt := range 2 {
			conn, err := l.AcceptUnix()
			if err != nil {
				done <- err
				return
			}
			_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
			challenge, err := security.NewGatewayAuthorityChallenge(startup.Binding)
			if err != nil {
				conn.Close()
				done <- err
				return
			}
			proof, err := security.SignGatewayAuthorityChallenge(challenge, f.source)
			if err == nil {
				err = writeAuthorityFixtureFrame(conn, startup.Binding.RuntimeID, proof)
			}
			var header [28]byte
			if err == nil {
				_, err = io.ReadFull(conn, header[:])
			}
			size := binary.BigEndian.Uint32(header[24:])
			if err != nil || string(header[:4]) != "RFGA" || size == 0 || size > security.MaxGatewayStatementSize {
				conn.Close()
				done <- security.ErrGatewayAuthority
				return
			}
			wire := make([]byte, size)
			if _, err = io.ReadFull(conn, wire); err != nil || f.authorization.Accept(wire) != nil {
				conn.Close()
				done <- security.ErrGatewayAuthority
				return
			}
			accepted = append(accepted, bytes.Clone(wire))
			if attempt == 1 {
				ack, signErr := security.SignGatewayAuthorityReceipt(security.GatewayAuthorityReceipt{Challenge: challenge, WireHash: security.GatewayPendingHash(wire)}, f.source)
				if signErr != nil || writeAuthorityFixtureFrame(conn, startup.Binding.RuntimeID, ack) != nil {
					conn.Close()
					done <- security.ErrGatewayAuthority
					return
				}
			}
			conn.Close() // First accepted statement deliberately loses its receipt.
		}
		done <- nil
	}()
	r := domain.GatewayDecisionRequest{ID: uuid.Must(uuid.NewV7()), AdmissionID: startup.Binding.AdmissionID, Owner: startup.Binding.Owner,
		RuntimeID: startup.Binding.RuntimeID, ExpectedRevision: 1, Lifetime: time.Minute}
	first := f.control.DeliverGatewayAuthorization(ctx, path, uint32(os.Geteuid()), startup.Binding, f.sourcePublic, r)
	second := f.control.DeliverGatewayAuthorization(ctx, path, uint32(os.Geteuid()), startup.Binding, f.sourcePublic, r)
	if err := <-done; err != nil || first != security.ErrGatewayAuthority || second != nil || len(accepted) != 2 || !bytes.Equal(accepted[0], accepted[1]) || f.authorization.Status() != gateway.AuthorizationValid {
		t.Fatal("receipt loss changed authority or replay issued a fresh grant")
	}
	startupCounts(t, f, 2, 1, 1)
}

func writeAuthorityFixtureFrame(w io.Writer, runtime uuid.UUID, body []byte) error {
	var header [28]byte
	copy(header[:4], "RFGA")
	binary.BigEndian.PutUint32(header[4:8], 1)
	copy(header[8:24], runtime[:])
	binary.BigEndian.PutUint32(header[24:], uint32(len(body)))
	_, err := io.Copy(w, io.MultiReader(bytes.NewReader(header[:]), bytes.NewReader(body)))
	return err
}
