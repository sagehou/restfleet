package httpapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sagehou/restfleet/internal/gateway"
	"github.com/sagehou/restfleet/internal/gatewaypending"
	"github.com/sagehou/restfleet/internal/security"
	"golang.org/x/crypto/nacl/box"
)

func TestGatewayMaterialCurrentTransactionAndProtectedDelivery(t *testing.T) {
	for _, backend := range []string{"onedrive", "drive", "webdav"} {
		t.Run(backend, func(t *testing.T) {
			f := pendingIntegrationFixture(t, backend)
			receiver, err := gatewaypending.NewMaterialReceiver(f.statement.Binding, f.source, f.centralPublic)
			if err != nil {
				t.Fatal(err)
			}
			defer receiver.Close()
			dir, err := os.MkdirTemp("", "rfg-material-")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(dir)
			path := filepath.Join(dir, "material.sock")
			l, err := gatewaypending.ListenReplay(path)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			done := make(chan error, 1)
			var borrowed []byte
			go func() {
				done <- receiver.Receive(ctx, l, uint32(os.Geteuid()), func(_ context.Context, m security.GatewayMaterial) (func(), error) {
					borrowed = m.Config
					if !bytes.Equal(m.Config, f.raw) || m.SecretRevision != 1 || m.PendingRecipient != f.recipient || m.Challenge.Binding != f.statement.Binding {
						t.Error("committed material changed")
					}
					a, err := gateway.NewAuthorization(f.centralPublic, m.Challenge.Binding)
					if err != nil || a.Accept(m.Statement) != nil || a.Status() != gateway.AuthorizationValid {
						return nil, security.ErrGatewayMaterial
					}
					return func() {}, nil
				}, f.control.RecordGatewayMaterialDenied)
			}()
			err = gatewaypending.SendMaterial(ctx, path, uint32(os.Geteuid()), f.statement.Binding, f.sourcePublic, func(ctx context.Context, challenge []byte) ([]byte, error) {
				wire, err := f.control.GatewayMaterialDelivery(ctx, f.statement.Binding, f.sourcePublic, challenge)
				if bytes.Contains(wire, f.raw) {
					t.Error("plaintext on transport")
				}
				return wire, err
			}, f.control.RecordGatewayMaterialDenied)
			if err != nil || <-done != nil || len(bytes.Trim(borrowed, "\x00")) != 0 {
				t.Fatal("current transaction delivery failed")
			}
			var accesses, intents int
			if err := f.pool.QueryRow(context.Background(), "select (select count(*) from audit_events where action='GATEWAY_MATERIAL_DELIVERY' and reason_code='SECRET_ACCESS'),(select count(*) from gateway_material_deliveries)").Scan(&accesses, &intents); err != nil || accesses != 1 || intents != 1 {
				t.Fatal("material access not audited exactly once")
			}
			f.count(t, 0, 0, 1)
		})
	}
}

func TestGatewayMaterialRefusesStaleStateAndAuditFailure(t *testing.T) {
	for _, failure := range []string{"owner", "source", "proof", "disabled", "revoked", "ack", "expired", "sealed", "started", "revision", "audit"} {
		t.Run(failure, func(t *testing.T) {
			f := pendingIntegrationFixture(t, "onedrive")
			ctx := context.Background()
			binding := f.statement.Binding
			source := f.sourcePublic
			recipient, _, _ := box.GenerateKey(rand.Reader)
			challenge, err := security.NewGatewayMaterialChallenge(binding, *recipient)
			if err != nil {
				t.Fatal(err)
			}
			proof, err := security.SignGatewayMaterialChallenge(challenge, f.source)
			if err != nil {
				t.Fatal(err)
			}
			switch failure {
			case "owner":
				binding.Owner = uuid.Must(uuid.NewV7())
			case "source":
				source = f.centralPublic
			case "proof":
				proof[len(proof)-1] ^= 1
			case "disabled":
				_, err = f.pool.Exec(ctx, "update storage_credentials set status='DISABLED' where id=$1", binding.StorageCredentialID)
			case "revoked":
				r := f.request
				r.ID = uuid.Must(uuid.NewV7())
				r.ExpectedRevision = 1
				r.Revoke = true
				r.Lifetime = 0
				_, err = f.control.DecideGatewayAuthorization(ctx, r)
			case "ack":
				_, err = f.pool.Exec(ctx, "update repository_agent_deliveries set accepted_at=null where id=$1", binding.DeliveryID)
			case "expired":
				_, err = f.pool.Exec(ctx, "update gateway_backup_admissions set created_at=clock_timestamp()-interval '20 minutes',expires_at=clock_timestamp()-interval '1 second' where id=$1", binding.AdmissionID)
			case "sealed":
				err = f.control.SealGatewayPending(ctx, binding.AdmissionID, binding.Owner, binding.RuntimeID, 0, f.record("audit").Header.PreviousHash)
			case "started":
				_, err = f.control.ReplayGatewayPending(ctx, binding.RuntimeID, f.seal(t, f.record("audit")))
			case "revision":
				_, err = f.pool.Exec(ctx, "update storage_credentials set secret_revision=secret_revision+1 where id=$1", binding.StorageCredentialID)
			case "audit":
				_, err = f.pool.Exec(ctx, `create function fail_material_audit() returns trigger language plpgsql as $$ begin if new.action='GATEWAY_MATERIAL_DELIVERY' then raise exception 'material-secret-canary';end if;return new;end $$;
				create trigger fail_material_audit before insert on audit_events for each row execute function fail_material_audit();`)
				t.Cleanup(func() {
					_, _ = f.pool.Exec(ctx, "drop trigger if exists fail_material_audit on audit_events;drop function if exists fail_material_audit()")
				})
			}
			if err != nil {
				t.Fatal("negative fixture could not establish state", err)
			}
			wire, err := f.control.GatewayMaterialDelivery(ctx, binding, source, proof)
			if !errors.Is(err, security.ErrGatewayMaterial) || wire != nil {
				t.Fatal("stale/unauthorized material released")
			}
			var accesses, denials, intents int
			if err = f.pool.QueryRow(ctx, `select (select count(*) from audit_events where action='GATEWAY_MATERIAL_DELIVERY'),
			(select count(*) from audit_events where action='GATEWAY_MATERIAL_DELIVERY_DENIED' and resource_id is null and changes='{}'::jsonb),
			(select count(*) from gateway_material_deliveries)`).Scan(&accesses, &denials, &intents); err != nil || accesses != 0 || denials == 0 || intents != 0 {
				t.Fatal("failed access committed or lacked safe rejection")
			}
		})
	}
}

func TestGatewayMaterialOneIntentLatestGrantAndUncertainRetry(t *testing.T) {
	f := pendingIntegrationFixture(t, "drive")
	r := f.request
	r.ID = uuid.Must(uuid.NewV7())
	r.ExpectedRevision = 1
	if _, err := f.control.DecideGatewayAuthorization(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	recipient, private, _ := box.GenerateKey(rand.Reader)
	challenge, err := security.NewGatewayMaterialChallenge(f.statement.Binding, *recipient)
	if err != nil {
		t.Fatal(err)
	}
	proof, err := security.SignGatewayMaterialChallenge(challenge, f.source)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan []byte, 4)
	for range 4 {
		wg.Go(func() {
			wire, err := f.control.GatewayMaterialDelivery(context.Background(), f.statement.Binding, f.sourcePublic, proof)
			if err == nil {
				results <- wire
			} else if err != security.ErrGatewayMaterial {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	close(results)
	var wire []byte
	for result := range results {
		if wire != nil {
			t.Fatal("concurrent initializations released multiple payloads")
		}
		wire = result
	}
	if wire == nil {
		t.Fatal("no delivery")
	}
	opened, err := security.OpenGatewayMaterial(wire, challenge, f.sourcePublic, f.centralPublic, private[:])
	if err != nil {
		t.Fatal(err)
	}
	defer clear(opened.Config)
	statement, err := security.VerifyGatewayStatement(opened.Statement, f.centralPublic)
	if err != nil || statement.Revision != 2 {
		t.Fatal("delivery did not include latest committed grant")
	}
	// No receipt was returned. A new signed challenge must still not reissue old
	// material or clear the intent; the unknown initialized owner retains fence.
	challenge, err = security.NewGatewayMaterialChallenge(f.statement.Binding, *recipient)
	if err != nil {
		t.Fatal(err)
	}
	proof, err = security.SignGatewayMaterialChallenge(challenge, f.source)
	if err != nil {
		t.Fatal(err)
	}
	if repeated, err := f.control.GatewayMaterialDelivery(context.Background(), f.statement.Binding, f.sourcePublic, proof); err != security.ErrGatewayMaterial || repeated != nil {
		t.Fatal("ACK loss reset material")
	}
	var intents, accesses int
	if err := f.pool.QueryRow(context.Background(), "select (select count(*) from gateway_material_deliveries),(select count(*) from audit_events where action='GATEWAY_MATERIAL_DELIVERY')").Scan(&intents, &accesses); err != nil || intents != 1 || accesses != 1 {
		t.Fatal("one intent/effect invariant")
	}
	if f.store.ReleaseBackupAdmission(context.Background(), f.statement.Binding.AdmissionID, f.statement.Binding.Owner) == nil {
		t.Fatal("unknown delivery released fence")
	}
}
