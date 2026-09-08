package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sagehou/restfleet/internal/domain"
	"github.com/sagehou/restfleet/internal/persistence/postgres"
	"github.com/sagehou/restfleet/internal/restic"
	"github.com/sagehou/restfleet/internal/security"
	control "github.com/sagehou/restfleet/internal/server"
)

func deliveryControl(t *testing.T, store *postgres.Store, ca []byte, origin string) *control.ControlPlane {
	t.Helper()
	c, err := control.NewControlPlane(store, control.Settings{MasterKey: bytes.Repeat([]byte{8}, 32), GatewayPublicURL: origin,
		Enrollment:     control.EnrollmentSettings{ServerCABundlePEM: ca},
		PasswordParams: security.Argon2Params{Memory: 64, Iterations: 1, Parallelism: 1, SaltLength: 8, KeyLength: 16}})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func deliveryFixture(t *testing.T, backend string) (*postgres.Store, *pgxpool.Pool, *testBrowser, AgentEnrollmentResponse, Repository, *control.ControlPlane) {
	t.Helper()
	store, pool, _, b, _ := setupFleet(t)
	credential := createBackendCredential(t, b, backend)
	host := createTestHost(t, b, "Credential Host")
	repo := createTestRepository(t, b, host.Id, credential.Id, "Credential Repository")
	token := createTestEnrollmentToken(t, b, host.Id)
	csr, key := agentCSR(t)
	clear(key)
	response := b.request(t, http.MethodPost, "/api/v1/agent-enrollment", enrollRequest(token.Token, csr, uuid.Must(uuid.NewV7())), nil)
	if response.Code != http.StatusCreated {
		t.Fatal("enrollment failed")
	}
	var enrolled AgentEnrollmentResponse
	decodeResponse(t, response, &enrolled)
	c := deliveryControl(t, store, []byte(enrolled.CaBundlePem), "https://gateway.example")
	return store, pool, b, enrolled, repo, c
}

func initializeDeliveryRepository(t *testing.T, store *postgres.Store, b *testBrowser, repo Repository) {
	t.Helper()
	queueInitialize(t, b, repo.Id, "prepare-delivery")
	worker := initializerWorker(t, store, func(context.Context, restic.ProvisionRequest, func(context.Context, []byte) error) (restic.RepositoryInfo, error) {
		return provisionInfo(), nil
	})
	if worked, err := worker.ProcessCredentialJob(context.Background(), uuid.Must(uuid.NewV7())); err != nil || !worked {
		t.Fatal("initialization failed")
	}
}

func TestAgentCredentialDeliveryDurableACKAndProviderIsolation(t *testing.T) {
	for _, backend := range []string{"onedrive", "drive", "webdav"} {
		t.Run(backend, func(t *testing.T) {
			store, pool, b, enrolled, repo, c := deliveryFixture(t, backend)
			ctx := context.Background()
			called := false
			if err := c.WithAgentCredential(ctx, enrolled.AgentId, true, func(domain.RepositoryCredential) error { called = true; return nil }); err != nil || called {
				t.Fatal("uninitialized repository delivered")
			}
			initializeDeliveryRepository(t, store, b, repo)
			var delivery domain.RepositoryCredential
			if err := c.WithAgentCredential(ctx, enrolled.AgentId, true, func(value domain.RepositoryCredential) error {
				called = true
				delivery = value
				if value.Validate() != nil || value.HostID != enrolled.HostId || value.RepositoryID != repo.Id || value.AgentID != enrolled.AgentId {
					t.Fatal("cross-Host credential or invalid material")
				}
				var audited int
				if err := pool.QueryRow(ctx, "select count(*) from audit_events where action='AGENT_REPOSITORY_SECRET_ACCESS' and request_id=$1", value.DeliveryID).Scan(&audited); err != nil || audited != 1 {
					t.Fatal("plaintext released before access audit")
				}
				encoded, _ := json.Marshal(value)
				defer clear(encoded)
				for _, canary := range []string{"storage-refresh-canary", "client-secret", "crypt-password", "webdav-password"} {
					if bytes.Contains(encoded, []byte(canary)) {
						t.Fatal("cloud secret reached Agent")
					}
				}
				return nil
			}); err != nil || !called {
				t.Fatal("delivery failed")
			}
			if !bytes.Equal(delivery.GatewayPassword, make([]byte, 43)) || !bytes.Equal(delivery.ResticPassword, make([]byte, 43)) {
				t.Fatal("borrowed plaintext not cleared")
			}
			for _, ack := range []struct {
				id       uuid.UUID
				revision int64
			}{{uuid.Must(uuid.NewV7()), 1}, {delivery.DeliveryID, 2}} {
				if c.AcceptAgentCredential(ctx, enrolled.AgentId, ack.id, ack.revision) == nil {
					t.Fatal("wrong ACK accepted")
				}
			}
			if c.AcceptAgentCredential(ctx, uuid.Must(uuid.NewV7()), delivery.DeliveryID, 1) == nil {
				t.Fatal("other Agent ACK accepted")
			}
			// A lost send/ACK reuses the exact durable delivery on the next attempt.
			if err := c.WithAgentCredential(ctx, enrolled.AgentId, false, func(value domain.RepositoryCredential) error {
				if value.DeliveryID != delivery.DeliveryID || value.Revision != 1 {
					t.Fatal("retry changed delivery identity")
				}
				return errors.New("test-secret-callback-canary")
			}); err == nil || strings.Contains(err.Error(), "canary") {
				t.Fatal("callback error not redacted")
			}
			for range 2 {
				if err := c.AcceptAgentCredential(ctx, enrolled.AgentId, delivery.DeliveryID, 1); err != nil {
					t.Fatal("exact/idempotent ACK failed")
				}
			}
			var audits int
			if err := pool.QueryRow(ctx, "select count(*) from audit_events where action='AGENT_REPOSITORY_CREDENTIAL_ACCEPTED'").Scan(&audits); err != nil || audits != 1 {
				t.Fatal("ACK audit not idempotent")
			}
			called = false
			if err := c.WithAgentCredential(ctx, enrolled.AgentId, false, func(domain.RepositoryCredential) error { called = true; return nil }); err != nil || called {
				t.Fatal("accepted delivery resent on heartbeat")
			}
			if err := c.WithAgentCredential(ctx, enrolled.AgentId, true, func(value domain.RepositoryCredential) error {
				called = true
				if value.DeliveryID != delivery.DeliveryID {
					t.Fatal("reconnect changed identity")
				}
				return nil
			}); err != nil || !called {
				t.Fatal("reconnect cannot recover lost local file")
			}
			response := b.request(t, http.MethodGet, "/api/v1/repositories/"+repo.Id.String(), nil, nil)
			var current Repository
			decodeResponse(t, response, &current)
			if current.AgentCredentialAcceptedAt == nil || current.AgentCredentialRevision == nil || *current.AgentCredentialRevision != 1 || current.Status != "PROVISIONING" {
				t.Fatal("ACK metadata missing or falsely marked READY")
			}
			assertRepositoryMetadata(t, response.Body.String())
			var pending int
			if err := pool.QueryRow(ctx, "select count(*) from outbox_events where event_type='AGENT_CREDENTIAL_CHANGED' and published_at is null").Scan(&pending); err != nil || pending != 0 {
				t.Fatal("ACK not atomic with outbox")
			}
			// An origin change requires a new revision, even if passwords are unchanged.
			next := deliveryControl(t, store, []byte(enrolled.CaBundlePem), "https://next-gateway.example")
			if next.AcceptAgentCredential(ctx, enrolled.AgentId, delivery.DeliveryID, 1) == nil {
				t.Fatal("old endpoint ACK accepted before reconciliation")
			}
			var nextID uuid.UUID
			if err := next.WithAgentCredential(ctx, enrolled.AgentId, false, func(value domain.RepositoryCredential) error {
				nextID = value.DeliveryID
				if value.Revision != 2 || value.GatewayRevision != 1 {
					t.Fatal("configuration change lost revisions")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if next.AcceptAgentCredential(ctx, enrolled.AgentId, delivery.DeliveryID, 1) == nil {
				t.Fatal("superseded ACK accepted")
			}
			if err := next.AcceptAgentCredential(ctx, enrolled.AgentId, nextID, 2); err != nil {
				t.Fatal(err)
			}
			if err := store.VerifyAuditChain(ctx); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestAgentCredentialDenialConcurrencyAndAuditRollback(t *testing.T) {
	store, pool, b, enrolled, repo, c := deliveryFixture(t, "onedrive")
	initializeDeliveryRepository(t, store, b, repo)
	ctx := context.Background()
	ids := make(chan uuid.UUID, 8)
	errs := make(chan error, 8)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			errs <- c.WithAgentCredential(ctx, enrolled.AgentId, true, func(value domain.RepositoryCredential) error { ids <- value.DeliveryID; return nil })
		})
	}
	wg.Wait()
	close(ids)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal("concurrent delivery failed")
		}
	}
	var id uuid.UUID
	for got := range ids {
		if id != uuid.Nil && id != got {
			t.Fatal("duplicate delivery allocated another revision")
		}
		id = got
	}
	for _, statement := range []string{
		"update hosts set status='DISABLED' where id=$1",
		"update agents set status='REVOKED' where host_id=$1",
		"update repositories set status='DISABLED' where host_id=$1",
	} {
		if _, err := pool.Exec(ctx, statement, enrolled.HostId); err != nil {
			t.Fatal(err)
		}
		called := false
		if err := c.WithAgentCredential(ctx, enrolled.AgentId, true, func(domain.RepositoryCredential) error { called = true; return nil }); err != nil || called {
			t.Fatal("disabled/revoked binding delivered")
		}
		if c.AcceptAgentCredential(ctx, enrolled.AgentId, id, 1) == nil {
			t.Fatal("disabled/revoked ACK accepted")
		}
		if _, err := pool.Exec(ctx, "update hosts set status='ACTIVE' where id=$1", enrolled.HostId); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, "update agents set status='ACTIVE' where id=$1", enrolled.AgentId); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, "update repositories set status='PROVISIONING' where id=$1", repo.Id); err != nil {
			t.Fatal(err)
		}
	}
	// Fail an audit INSERT: neither a new delivery nor its outbox may commit.
	_, err := pool.Exec(ctx, `create function reject_agent_delivery_audit() returns trigger language plpgsql as $$ begin
		if new.action='AGENT_REPOSITORY_SECRET_ACCESS' then raise exception 'audit unavailable'; end if; return new; end $$;
		create trigger reject_agent_delivery before insert on audit_events for each row execute function reject_agent_delivery_audit();`)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "drop trigger if exists reject_agent_delivery on audit_events; drop function if exists reject_agent_delivery_audit()")
	})
	changed := deliveryControl(t, store, []byte(enrolled.CaBundlePem), "https://changed.example")
	called := false
	if err := changed.WithAgentCredential(ctx, enrolled.AgentId, true, func(domain.RepositoryCredential) error { called = true; return nil }); err == nil || called {
		t.Fatal("audit failure released secrets")
	}
	var revision int
	if err := pool.QueryRow(ctx, "select revision from repository_agent_deliveries where agent_id=$1", enrolled.AgentId).Scan(&revision); err != nil || revision != 1 {
		t.Fatal("failed audit committed revision")
	}
}
