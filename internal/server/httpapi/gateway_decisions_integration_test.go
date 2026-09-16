package httpapi

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sagehou/restfleet/internal/domain"
	"github.com/sagehou/restfleet/internal/gateway"
	"github.com/sagehou/restfleet/internal/persistence/postgres"
	"github.com/sagehou/restfleet/internal/security"
	control "github.com/sagehou/restfleet/internal/server"
)

func gatewayDecisionFixture(t *testing.T, backend string) (*postgres.Store, *pgxpool.Pool, *control.ControlPlane, domain.GatewayDecisionRequest, ed25519.PublicKey) {
	t.Helper()
	s, pool, b, enrolled, repo, c := deliveryFixture(t, backend)
	initializeDeliveryRepository(t, s, b, repo)
	a := acceptedAdmissionRequest(t, pool, c, enrolled.AgentId)
	if _, err := s.ReserveBackupAdmission(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	c, err = control.NewControlPlane(s, control.Settings{MasterKey: bytes.Repeat([]byte{8}, 32), GatewayPublicURL: "https://gateway.example",
		GatewaySigningKey: private, Enrollment: control.EnrollmentSettings{ServerCABundlePEM: []byte(enrolled.CaBundlePem)},
		PasswordParams: security.Argon2Params{Memory: 64, Iterations: 1, Parallelism: 1, SaltLength: 8, KeyLength: 16}})
	clear(private)
	if err != nil {
		t.Fatal(err)
	}
	r := domain.GatewayDecisionRequest{ID: uuid.Must(uuid.NewV7()), AdmissionID: a.ID, Owner: a.Owner, RuntimeID: uuid.Must(uuid.NewV7()), Lifetime: 10 * time.Minute}
	return s, pool, c, r, public
}

func TestGatewayDecisionProvidersRenewalReplayAndWithdrawal(t *testing.T) {
	for _, backend := range []string{"onedrive", "drive", "webdav"} {
		t.Run(backend, func(t *testing.T) {
			s, pool, c, r, key := gatewayDecisionFixture(t, backend)
			ctx := context.Background()
			wire, err := c.DecideGatewayAuthorization(ctx, r)
			if err != nil {
				t.Fatal(err)
			}
			first, err := security.VerifyGatewayStatement(wire, key)
			if err != nil || first.Revision != 1 || first.Binding.AdmissionID != r.AdmissionID || first.Binding.RuntimeID != r.RuntimeID {
				t.Fatal("bad signed binding")
			}
			var count, audits, events int
			if err = pool.QueryRow(ctx, "select count(*) from gateway_authorization_decisions").Scan(&count); err != nil || count != 1 {
				t.Fatal("signed before durable decision")
			}
			local, err := gateway.NewAuthorization(key, first.Binding)
			if err != nil || local.Accept(wire) != nil || local.Status() != gateway.AuthorizationValid {
				t.Fatal("Gateway rejected committed grant")
			}
			for range 2 {
				replay, err := c.DecideGatewayAuthorization(ctx, r)
				if err != nil || !bytes.Equal(replay, wire) {
					t.Fatal("replay extended or changed authorization")
				}
			}
			if err = s.ReleaseBackupAdmission(ctx, r.AdmissionID, r.Owner); !errors.Is(err, domain.ErrGatewayDecision) {
				t.Fatal("outstanding grant lost its fence")
			}
			renew := r
			renew.ID = uuid.Must(uuid.NewV7())
			renew.ExpectedRevision = 1
			renew.Lifetime = 12 * time.Hour
			nextWire, err := c.DecideGatewayAuthorization(ctx, renew)
			if err != nil {
				t.Fatal(err)
			}
			next, err := security.VerifyGatewayStatement(nextWire, key)
			if err != nil || next.Revision != 2 || next.Binding != first.Binding || next.ExpiresAt <= first.ExpiresAt {
				t.Fatal("renewal changed binding or failed to extend")
			}
			var expires time.Time
			var released *time.Time
			if err = pool.QueryRow(ctx, "select expires_at,released_at from gateway_backup_admissions where id=$1", r.AdmissionID).Scan(&expires, &released); err != nil || released != nil || next.ExpiresAt != expires.Truncate(time.Second).Unix() {
				t.Fatal("renewal escaped original fence horizon")
			}
			if local.Accept(nextWire) != nil || local.Accept(wire) == nil {
				t.Fatal("revision rollback accepted")
			}
			if raw, err := c.DecideGatewayAuthorization(ctx, r); err == nil || raw != nil {
				t.Fatal("superseded idempotency key reissued")
			}
			if err = pool.QueryRow(ctx, "select count(*) from audit_events where action='GATEWAY_AUTHORIZATION_DECISION'").Scan(&audits); err != nil || audits != 2 {
				t.Fatal("duplicate/missing decision audit")
			}
			if err = pool.QueryRow(ctx, "select count(*) from outbox_events where event_type='GATEWAY_AUTHORIZATION_DECIDED'").Scan(&events); err != nil || events != 2 {
				t.Fatal("duplicate/missing outbox")
			}
			// Revocation must remain available after the principal is disabled.
			if _, err = pool.Exec(ctx, "update agents set status='REVOKED'"); err != nil {
				t.Fatal(err)
			}
			withdraw := renew
			withdraw.ID = uuid.Must(uuid.NewV7())
			withdraw.ExpectedRevision = 2
			withdraw.Revoke = true
			withdraw.Lifetime = 0
			revoked, err := c.DecideGatewayAuthorization(ctx, withdraw)
			if err != nil || local.Accept(revoked) != nil || local.Status() != gateway.AuthorizationRevoked {
				t.Fatal("explicit withdrawal failed")
			}
			if _, err = pool.Exec(ctx, "update agents set status='ACTIVE'"); err != nil {
				t.Fatal(err)
			}
			if _, err = s.CheckBackupAdmission(ctx, r.AdmissionID, r.Owner, first.Binding.ConfigurationHash); err == nil {
				t.Fatal("known revocation ignored by online check")
			}
			if _, err = s.BackupAdmissionMaterial(ctx, r.AdmissionID, r.Owner, first.Binding.ConfigurationHash); err == nil {
				t.Fatal("revoked binding obtained material")
			}
			if err = pool.QueryRow(ctx, "select released_at from gateway_backup_admissions where id=$1", r.AdmissionID).Scan(&released); err != nil || released != nil {
				t.Fatal("withdrawal pretended cleanup")
			}
			if err = s.ReleaseBackupAdmission(ctx, r.AdmissionID, r.Owner); err != nil {
				t.Fatal(err)
			}
			if repeat, err := c.DecideGatewayAuthorization(ctx, withdraw); err != nil || !bytes.Equal(repeat, revoked) {
				t.Fatal("withdrawal replay not idempotent")
			}
			if err = s.VerifyAuditChain(ctx); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestGatewayDecisionConcurrentCASAndBinding(t *testing.T) {
	s, pool, c, r, _ := gatewayDecisionFixture(t, "onedrive")
	ctx := context.Background()
	if _, err := c.DecideGatewayAuthorization(ctx, r); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 8)
	wg.Go(func() {
		if err := s.ReleaseBackupAdmission(ctx, r.AdmissionID, r.Owner); err != domain.ErrGatewayDecision {
			t.Error("concurrent release bypassed live grant")
		}
	})
	for range 8 {
		wg.Go(func() {
			next := r
			next.ID = uuid.Must(uuid.NewV7())
			next.ExpectedRevision = 1
			_, err := c.DecideGatewayAuthorization(ctx, next)
			results <- err
		})
	}
	wg.Wait()
	close(results)
	winners := 0
	for err := range results {
		if err == nil {
			winners++
		}
	}
	if winners != 1 {
		t.Fatal("CAS admitted multiple revisions")
	}
	for _, mutate := range []func(*domain.GatewayDecisionRequest){
		func(r *domain.GatewayDecisionRequest) { r.RuntimeID = uuid.Must(uuid.NewV7()) },
		func(r *domain.GatewayDecisionRequest) { r.Owner = uuid.Must(uuid.NewV7()) },
		func(r *domain.GatewayDecisionRequest) { r.AdmissionID = uuid.Must(uuid.NewV7()) },
	} {
		bad := r
		bad.ID = uuid.Must(uuid.NewV7())
		bad.ExpectedRevision = 2
		mutate(&bad)
		if raw, err := c.DecideGatewayAuthorization(ctx, bad); err == nil || raw != nil {
			t.Fatal("foreign binding authorized")
		}
	}
	var count int
	if err := pool.QueryRow(ctx, "select count(*) from gateway_authorization_decisions").Scan(&count); err != nil || count != 2 {
		t.Fatal("failed CAS wrote history")
	}
}

func TestGatewayDecisionInvalidCurrentState(t *testing.T) {
	_, pool, c, r, _ := gatewayDecisionFixture(t, "onedrive")
	ctx := context.Background()
	for _, sql := range []string{
		"update repository_agent_deliveries set accepted_at=null",
		"update agents set status='REVOKED'", "update hosts set status='DISABLED'",
		"update repositories set status='LOCKED'", "update storage_credentials set status='DISABLED'",
	} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
		if raw, err := c.DecideGatewayAuthorization(ctx, r); err == nil || raw != nil {
			t.Fatal("invalid current state signed")
		}
		if _, err := pool.Exec(ctx, "update repository_agent_deliveries set accepted_at=clock_timestamp();update agents set status='ACTIVE';update hosts set status='ACTIVE';update repositories set status='PROVISIONING';update storage_credentials set status='UNTESTED'"); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err := pool.QueryRow(ctx, "select count(*) from gateway_authorization_decisions").Scan(&count); err != nil || count != 0 {
		t.Fatal("denial persisted authority")
	}
}

func TestGatewayDecisionAuditFailureRollsBackBeforeSigning(t *testing.T) {
	_, pool, c, r, _ := gatewayDecisionFixture(t, "onedrive")
	ctx := context.Background()
	_, err := pool.Exec(ctx, `create function fail_gateway_decision_audit() returns trigger language plpgsql as $$ begin
		if new.action='GATEWAY_AUTHORIZATION_DECISION' then raise exception 'audit-failure-canary'; end if; return new; end $$;
		create trigger fail_gateway_decision_audit before insert on audit_events for each row execute function fail_gateway_decision_audit();`)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "drop trigger if exists fail_gateway_decision_audit on audit_events;drop function if exists fail_gateway_decision_audit()")
	})
	if raw, err := c.DecideGatewayAuthorization(ctx, r); err != domain.ErrGatewayDecision || raw != nil {
		t.Fatal("failed audit leaked signature/error")
	}
	var decisions, events int
	if err = pool.QueryRow(ctx, "select (select count(*) from gateway_authorization_decisions),(select count(*) from outbox_events where event_type='GATEWAY_AUTHORIZATION_DECIDED')").Scan(&decisions, &events); err != nil || decisions != 0 || events != 0 {
		t.Fatal("audit failure left partial authority")
	}
}

func TestGatewayDecisionAuditWaitCannotConsumeLifetime(t *testing.T) {
	_, pool, c, r, _ := gatewayDecisionFixture(t, "onedrive")
	ctx := context.Background()
	_, err := pool.Exec(ctx, `create function delay_gateway_decision_audit() returns trigger language plpgsql as $$ begin
		if new.action='GATEWAY_AUTHORIZATION_DECISION' then perform pg_sleep(1.1); end if; return new; end $$;
		create trigger delay_gateway_decision_audit before insert on audit_events for each row execute function delay_gateway_decision_audit();`)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "drop trigger if exists delay_gateway_decision_audit on audit_events;drop function if exists delay_gateway_decision_audit()")
	})
	r.Lifetime = time.Second
	if raw, err := c.DecideGatewayAuthorization(ctx, r); err != domain.ErrGatewayDecision || raw != nil {
		t.Fatal("authorization escaped after audit consumed its lifetime")
	}
	var count int
	if err = pool.QueryRow(ctx, "select count(*) from gateway_authorization_decisions").Scan(&count); err != nil || count != 0 {
		t.Fatal("expired transaction committed a grant")
	}
}

func TestGatewayDecisionExpiredAdmissionCannotRenewOrLoseFence(t *testing.T) {
	s, pool, c, r, key := gatewayDecisionFixture(t, "onedrive")
	ctx := context.Background()
	wire, err := c.DecideGatewayAuthorization(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	first, err := security.VerifyGatewayStatement(wire, key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, "update gateway_backup_admissions set created_at=clock_timestamp()-interval '2 hours',expires_at=clock_timestamp()-interval '1 hour' where id=$1", r.AdmissionID); err != nil {
		t.Fatal(err)
	}
	next := r
	next.ID = uuid.Must(uuid.NewV7())
	next.ExpectedRevision = 1
	if raw, err := c.DecideGatewayAuthorization(ctx, next); err == nil || raw != nil {
		t.Fatal("expired fence renewed")
	}
	if err = s.ReleaseBackupAdmission(ctx, r.AdmissionID, r.Owner); err != domain.ErrGatewayDecision {
		t.Fatal("live signed grant lost fence on admission expiry")
	}
	other := domain.BackupAdmissionRequest{ID: uuid.Must(uuid.NewV7()), Owner: uuid.Must(uuid.NewV7()), AgentID: first.Binding.AgentID,
		DeliveryID: first.Binding.DeliveryID, ConfigurationHash: first.Binding.ConfigurationHash, Lifetime: time.Hour}
	if _, err = s.ReserveBackupAdmission(ctx, other); err != domain.ErrRepositoryBusy {
		t.Fatal("expired fence replaced without cleanup")
	}
	next.Revoke = true
	next.Lifetime = 0
	if _, err = c.DecideGatewayAuthorization(ctx, next); err != nil {
		t.Fatal("expired admission cannot be withdrawn")
	}
	var released *time.Time
	if err = pool.QueryRow(ctx, "select released_at from gateway_backup_admissions where id=$1", r.AdmissionID).Scan(&released); err != nil || released != nil {
		t.Fatal("withdrawal released fence")
	}
}

func TestGatewayDecisionConcurrentExactReplay(t *testing.T) {
	_, pool, c, r, _ := gatewayDecisionFixture(t, "onedrive")
	ctx := context.Background()
	var wg sync.WaitGroup
	wires := make(chan []byte, 8)
	for range 8 {
		wg.Go(func() {
			wire, err := c.DecideGatewayAuthorization(ctx, r)
			if err != nil {
				t.Error(err)
			}
			wires <- wire
		})
	}
	wg.Wait()
	close(wires)
	var first []byte
	for wire := range wires {
		if len(wire) == 0 {
			t.Fatal("replay failed")
		}
		if first == nil {
			first = wire
		} else if !bytes.Equal(first, wire) {
			t.Fatal("concurrent replay diverged")
		}
	}
	var decisions, audits int
	if err := pool.QueryRow(ctx, "select (select count(*) from gateway_authorization_decisions),(select count(*) from audit_events where action='GATEWAY_AUTHORIZATION_DECISION')").Scan(&decisions, &audits); err != nil || decisions != 1 || audits != 1 {
		t.Fatal("duplicate replay created history")
	}
}
