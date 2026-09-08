package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sagehou/restfleet/internal/domain"
	"github.com/sagehou/restfleet/internal/persistence/postgres"
	control "github.com/sagehou/restfleet/internal/server"
)

func admissionFixture(t *testing.T, backend string) (*postgres.Store, *pgxpool.Pool, *testBrowser, Repository, domain.BackupAdmissionRequest) {
	t.Helper()
	s, pool, b, enrolled, repo, c := deliveryFixture(t, backend)
	initializeDeliveryRepository(t, s, b, repo)
	request := acceptedAdmissionRequest(t, pool, c, enrolled.AgentId)
	return s, pool, b, repo, request
}

func acceptedAdmissionRequest(t *testing.T, pool *pgxpool.Pool, c *control.ControlPlane, agentID uuid.UUID) domain.BackupAdmissionRequest {
	t.Helper()
	ctx := context.Background()
	r := domain.BackupAdmissionRequest{ID: uuid.Must(uuid.NewV7()), Owner: uuid.Must(uuid.NewV7()), AgentID: agentID, Lifetime: time.Hour}
	var revision int64
	if err := c.WithAgentCredential(ctx, agentID, true, func(d domain.RepositoryCredential) error {
		r.DeliveryID, revision = d.DeliveryID, d.Revision
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := c.AcceptAgentCredential(ctx, agentID, r.DeliveryID, revision); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, "select configuration_hash from repository_agent_deliveries where agent_id=$1", agentID).Scan(&r.ConfigurationHash); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestBackupAdmissionProvidersReplayAndRelease(t *testing.T) {
	for _, backend := range []string{"onedrive", "drive", "webdav"} {
		t.Run(backend, func(t *testing.T) {
			s, pool, _, repo, request := admissionFixture(t, backend)
			ctx := context.Background()
			a, err := s.ReserveBackupAdmission(ctx, request)
			if err != nil {
				t.Fatal(err)
			}
			if a.ID != request.ID || a.RepositoryID != repo.Id || a.AgentID != request.AgentID || a.StorageCredentialID != repo.StorageCredentialId || a.ExpiresAt.Sub(a.CreatedAt) != time.Hour {
				t.Fatal("invalid binding/deadline")
			}
			for range 2 {
				replay, err := s.ReserveBackupAdmission(ctx, request)
				if err != nil || !replay.ExpiresAt.Equal(a.ExpiresAt) || replay.ID != a.ID {
					t.Fatal("replay changed admission")
				}
			}
			for _, mutate := range []func(*domain.BackupAdmissionRequest){func(r *domain.BackupAdmissionRequest) { r.Owner = uuid.Must(uuid.NewV7()) }, func(r *domain.BackupAdmissionRequest) { r.Lifetime = 2 * time.Hour }, func(r *domain.BackupAdmissionRequest) { r.DeliveryID = uuid.Must(uuid.NewV7()) }} {
				bad := request
				mutate(&bad)
				if _, err := s.ReserveBackupAdmission(ctx, bad); !errors.Is(err, domain.ErrBackupAdmission) {
					t.Fatal("changed replay accepted")
				}
			}
			// A new store instance uses the same authoritative row, no in-memory queue.
			restarted, err := postgres.Open(ctx, os.Getenv("RESTFLEET_TEST_DATABASE_URL"))
			if err != nil {
				t.Fatal(err)
			}
			defer restarted.Close()
			if _, err := restarted.CheckBackupAdmission(ctx, a.ID, a.Owner, request.ConfigurationHash); err != nil {
				t.Fatal(err)
			}
			if _, err := s.CheckBackupAdmission(ctx, a.ID, uuid.Must(uuid.NewV7()), request.ConfigurationHash); !errors.Is(err, domain.ErrBackupAdmission) {
				t.Fatal("other owner accepted")
			}
			if _, err := s.CheckBackupAdmission(ctx, a.ID, a.Owner, strings.Repeat("b", 64)); !errors.Is(err, domain.ErrBackupAdmission) {
				t.Fatal("changed gateway origin/CA accepted")
			}
			if err := s.ReleaseBackupAdmission(ctx, a.ID, uuid.Must(uuid.NewV7())); !errors.Is(err, domain.ErrBackupAdmission) {
				t.Fatal("other owner released reservation")
			}
			for range 2 {
				if err := restarted.ReleaseBackupAdmission(ctx, a.ID, a.Owner); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.ReserveBackupAdmission(ctx, request); !errors.Is(err, domain.ErrBackupAdmission) {
				t.Fatal("released admission reopened")
			}
			var admitted, released, events, operations int
			if err := pool.QueryRow(ctx, `select count(*) filter(where action='GATEWAY_BACKUP_ADMISSION'),count(*) filter(where action='GATEWAY_BACKUP_RELEASE') from audit_events`).Scan(&admitted, &released); err != nil {
				t.Fatal(err)
			}
			if err := pool.QueryRow(ctx, "select count(*) from outbox_events where aggregate_type='GATEWAY_ADMISSION'").Scan(&events); err != nil {
				t.Fatal(err)
			}
			if err := pool.QueryRow(ctx, "select count(*) from operations").Scan(&operations); err != nil {
				t.Fatal(err)
			}
			if admitted != 1 || released != 1 || events != 2 || operations != 1 {
				t.Fatal("duplicate work, missing audit or fabricated backup result")
			}
			var raw string
			if err := pool.QueryRow(ctx, "select row_to_json(a)::text from gateway_backup_admissions a where id=$1", a.ID).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			for _, canary := range []string{"storage-access-canary", "storage-refresh-canary", "webdav-bearer-canary", "google-secret-canary", "ciphertext", "rclone_config"} {
				if strings.Contains(raw, canary) {
					t.Fatal("secret entered admission metadata")
				}
			}
			if err := s.VerifyAuditChain(ctx); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestBackupAdmissionExpiryStillFencesCentralWriters(t *testing.T) {
	s, pool, b, repo, request := admissionFixture(t, "onedrive")
	ctx := context.Background()
	a, err := s.ReserveBackupAdmission(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	// Expiry revokes use, NOT evidence that the old process stopped writing.
	if _, err = pool.Exec(ctx, "update gateway_backup_admissions set created_at=clock_timestamp()-interval '2 hours',expires_at=clock_timestamp()-interval '1 hour' where id=$1", a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.CheckBackupAdmission(ctx, a.ID, a.Owner, request.ConfigurationHash); !errors.Is(err, domain.ErrBackupAdmission) {
		t.Fatal("expired admission usable")
	}
	next := request
	next.ID = uuid.Must(uuid.NewV7())
	next.Owner = uuid.Must(uuid.NewV7())
	if _, err = s.ReserveBackupAdmission(ctx, next); !errors.Is(err, domain.ErrRepositoryBusy) {
		t.Fatal("expired owner silently replaced")
	}
	credential, err := s.StorageCredential(ctx, repo.StorageCredentialId)
	if err != nil {
		t.Fatal(err)
	}
	headers := map[string]string{"X-CSRF-Token": b.cookies[csrfCookieName].Value, "If-Match": fmt.Sprintf("\"%d\"", credential.Revision), "Idempotency-Key": "blocked-by-backup"}
	path := "/api/v1/storage-credentials/" + credential.ID.String()
	for _, endpoint := range []string{path + "/test", path + "/replace-secret"} {
		var body any
		if strings.HasSuffix(endpoint, "replace-secret") {
			body = StorageCredentialReplace{RcloneConfig: credentialConfig(credentialFixture())}
		}
		response := b.request(t, http.MethodPost, endpoint, body, headers)
		if response.Code != 409 || !strings.Contains(response.Body.String(), "REPOSITORY_BUSY") {
			t.Fatalf("writer not fenced: %d", response.Code)
		}
		assertNoStorageSecret(t, response.Body.String())
	}
	// A sibling repository shares the credential, so initialization is also fenced.
	host := createTestHost(t, b, "Sibling Host")
	sibling := createTestRepository(t, b, host.Id, credential.ID, "Sibling Repository")
	if b.request(t, http.MethodPost, "/api/v1/repositories/"+sibling.Id.String()+"/initialize", nil, headers).Code != 409 {
		t.Fatal("sibling initialization bypassed reservation")
	}
	if err = s.ReleaseBackupAdmission(ctx, a.ID, a.Owner); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ReserveBackupAdmission(ctx, next); err != nil {
		t.Fatal("confirmed cleanup did not release capacity")
	}
	// Revocation remains possible, but cannot pretend cleanup has happened.
	if b.request(t, http.MethodPost, path+"/disable", nil, headers).Code != 200 {
		t.Fatal("emergency credential disable blocked")
	}
	if _, err = s.CheckBackupAdmission(ctx, next.ID, next.Owner, next.ConfigurationHash); !errors.Is(err, domain.ErrBackupAdmission) {
		t.Fatal("disabled credential remained authorized")
	}
	if err = s.ReleaseBackupAdmission(ctx, next.ID, next.Owner); err != nil {
		t.Fatal("revoked binding prevented trusted cleanup")
	}
}

func TestBackupAdmissionRejectsInvalidBindingAndMissingACK(t *testing.T) {
	s, pool, _, repo, request := admissionFixture(t, "onedrive")
	ctx := context.Background()
	for _, sql := range []string{
		"update repository_agent_deliveries set accepted_at=null",
		"update hosts set status='DISABLED'",
		"update agents set status='REVOKED'",
		"update repositories set status='LOCKED'",
		"update repositories set status='DISABLED'",
		"update storage_credentials set status='DISABLED'",
	} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ReserveBackupAdmission(ctx, request); !errors.Is(err, domain.ErrBackupAdmission) {
			t.Fatal("invalid binding admitted")
		}
		if _, err := pool.Exec(ctx, "update repository_agent_deliveries set accepted_at=clock_timestamp();update hosts set status='ACTIVE';update agents set status='ACTIVE';update repositories set status='PROVISIONING';update storage_credentials set status='UNTESTED'"); err != nil {
			t.Fatal(err)
		}
	}
	other := request
	other.AgentID = uuid.Must(uuid.NewV7())
	if _, err := s.ReserveBackupAdmission(ctx, other); !errors.Is(err, domain.ErrBackupAdmission) {
		t.Fatal("other Agent selected repository")
	}
	a, err := s.ReserveBackupAdmission(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "update repository_agent_deliveries set id=$1 where agent_id=$2", uuid.Must(uuid.NewV7()), request.AgentID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CheckBackupAdmission(ctx, a.ID, a.Owner, request.ConfigurationHash); !errors.Is(err, domain.ErrBackupAdmission) {
		t.Fatal("superseded credential ACK accepted")
	}
	var status string
	if err := pool.QueryRow(ctx, "select status from repositories where id=$1", repo.Id).Scan(&status); err != nil || status != "PROVISIONING" {
		t.Fatal("admission falsely marked repository READY")
	}
}

func TestBackupAdmissionConcurrencyAndPendingJobExclusion(t *testing.T) {
	s, pool, b, repo, request := admissionFixture(t, "onedrive")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	host := createTestHost(t, b, "Concurrent sibling")
	sibling := createTestRepository(t, b, host.Id, repo.StorageCredentialId, "Concurrent sibling repository")
	initializeDeliveryRepository(t, s, b, sibling)
	token := createTestEnrollmentToken(t, b, host.Id)
	csr, key := agentCSR(t)
	clear(key)
	response := b.request(t, http.MethodPost, "/api/v1/agent-enrollment", enrollRequest(token.Token, csr, uuid.Must(uuid.NewV7())), nil)
	if response.Code != 201 {
		t.Fatal("sibling enrollment failed")
	}
	var enrolled AgentEnrollmentResponse
	decodeResponse(t, response, &enrolled)
	other := acceptedAdmissionRequest(t, pool, deliveryControl(t, s, []byte(enrolled.CaBundlePem), "https://gateway.example"), enrolled.AgentId)
	queueCredentialTest(t, b, repo.StorageCredentialId, "before-backup")
	if _, err := s.ReserveBackupAdmission(ctx, request); !errors.Is(err, domain.ErrRepositoryBusy) {
		t.Fatal("pending job did not block admission")
	}
	worker := credentialWorker(t, s, func(context.Context, []byte, string, func(context.Context, []byte) error) error { return nil })
	if _, err := worker.ProcessCredentialJob(ctx, uuid.Must(uuid.NewV7())); err != nil {
		t.Fatal(err)
	}
	// Even a completed operation's still-valid legacy lease must be respected.
	if _, err := pool.Exec(ctx, "update repository_leases set expires_at=clock_timestamp()+interval '1 minute' where repository_id=$1", repo.Id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReserveBackupAdmission(ctx, request); !errors.Is(err, domain.ErrRepositoryBusy) {
		t.Fatal("maintenance lease ignored")
	}
	if _, err := pool.Exec(ctx, "update repository_leases set expires_at=clock_timestamp()-interval '1 second'"); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 8)
	for range 8 {
		wg.Go(func() { _, err := s.ReserveBackupAdmission(ctx, request); results <- err })
	}
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal("exact concurrent replay failed")
		}
	}
	if err := s.ReleaseBackupAdmission(ctx, request.ID, request.Owner); err != nil {
		t.Fatal(err)
	}
	winners := make(chan domain.BackupAdmission, 8)
	failures := make(chan error, 8)
	for i := range 8 {
		wg.Go(func() {
			r := request
			if i%2 == 1 {
				r = other
			}
			r.ID = uuid.Must(uuid.NewV7())
			a, err := s.ReserveBackupAdmission(ctx, r)
			if err == nil {
				winners <- a
			} else {
				failures <- err
			}
		})
	}
	wg.Wait()
	close(winners)
	close(failures)
	if len(winners) != 1 {
		t.Fatal("multiple owners admitted")
	}
	for err := range failures {
		if !errors.Is(err, domain.ErrRepositoryBusy) {
			t.Fatal(err)
		}
	}
	a := <-winners
	// Inject a legacy/pre-existing queued job to prove CLAIM also checks, even
	// if enqueue validation was skipped by an earlier binary or interrupted flow.
	jobID, opID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	_, err := pool.Exec(ctx, `insert into operations(id,type,status,source,storage_credential_id,secret_revision,requested_by_user_id,attempt,created_at)
		select $1,'CREDENTIAL_TEST','QUEUED','USER',id,secret_revision,(select id from users limit 1),1,clock_timestamp() from storage_credentials where id=$2;
		`, opID, repo.StorageCredentialId)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, "insert into jobs(id,operation_id,queue,status,available_at,created_at,updated_at) values($1,$2,'CREDENTIAL_TEST','READY',clock_timestamp(),clock_timestamp(),clock_timestamp())", jobID, opID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ClaimCredentialJob(ctx, uuid.Must(uuid.NewV7())); !errors.Is(err, domain.ErrRepositoryBusy) {
		t.Fatal("claim bypassed active admission")
	}
	if err = s.ReleaseBackupAdmission(ctx, a.ID, a.Owner); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ClaimCredentialJob(ctx, uuid.Must(uuid.NewV7())); err != nil {
		t.Fatal("cleanup did not unblock pending job")
	}
}

func TestBackupAdmissionAuditRollback(t *testing.T) {
	s, pool, _, _, request := admissionFixture(t, "onedrive")
	ctx := context.Background()
	_, err := pool.Exec(ctx, `create function reject_backup_admission_audit() returns trigger language plpgsql as $$ begin
		if new.action in ('GATEWAY_BACKUP_ADMISSION','GATEWAY_BACKUP_RELEASE') then raise exception 'audit unavailable'; end if; return new; end $$;
		create trigger reject_backup_admission before insert on audit_events for each row execute function reject_backup_admission_audit();`)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "drop trigger if exists reject_backup_admission on audit_events;drop function if exists reject_backup_admission_audit()")
	})
	if _, err = s.ReserveBackupAdmission(ctx, request); err == nil {
		t.Fatal("audit failure admitted")
	}
	var count int
	if err = pool.QueryRow(ctx, "select count(*) from gateway_backup_admissions").Scan(&count); err != nil || count != 0 {
		t.Fatal("failed admission left row")
	}
	if err = pool.QueryRow(ctx, "select count(*) from outbox_events where aggregate_type='GATEWAY_ADMISSION'").Scan(&count); err != nil || count != 0 {
		t.Fatal("failed admission left outbox")
	}
	if _, err = pool.Exec(ctx, "alter table audit_events disable trigger reject_backup_admission"); err != nil {
		t.Fatal(err)
	}
	a, err := s.ReserveBackupAdmission(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, "alter table audit_events enable trigger reject_backup_admission"); err != nil {
		t.Fatal(err)
	}
	if err = s.ReleaseBackupAdmission(ctx, a.ID, a.Owner); err == nil {
		t.Fatal("release without audit committed")
	}
	var released bool
	if err = pool.QueryRow(ctx, "select released_at is not null from gateway_backup_admissions where id=$1", a.ID).Scan(&released); err != nil || released {
		t.Fatal("failed release dropped fence")
	}
	var payload []byte
	if err = pool.QueryRow(ctx, "select payload from outbox_events where aggregate_id=$1", a.ID).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	var metadata map[string]any
	if json.Unmarshal(payload, &metadata) != nil || len(metadata) != 0 {
		t.Fatal("admission dispatch leaked material")
	}
}

func TestBackupAdmissionRechecksDeadlineAfterAudit(t *testing.T) {
	s, pool, _, _, request := admissionFixture(t, "onedrive")
	ctx := context.Background()
	_, err := pool.Exec(ctx, `create function expire_backup_during_audit() returns trigger language plpgsql as $$ begin
  if new.action='GATEWAY_BACKUP_ADMISSION' then
   update gateway_backup_admissions set created_at=clock_timestamp()-interval '2 hours',expires_at=clock_timestamp()-interval '1 hour' where id=new.request_id;
  end if; return new; end $$;
  create trigger expire_backup_admission before insert on audit_events for each row execute function expire_backup_during_audit();`)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "drop trigger if exists expire_backup_admission on audit_events;drop function if exists expire_backup_during_audit()")
	})
	if _, err = s.ReserveBackupAdmission(ctx, request); !errors.Is(err, domain.ErrBackupAdmission) {
		t.Fatal("audit wait bypassed deadline fence")
	}
	var count int
	if err = pool.QueryRow(ctx, "select count(*) from gateway_backup_admissions").Scan(&count); err != nil || count != 0 {
		t.Fatal("expired admission partially committed")
	}
	if err = pool.QueryRow(ctx, "select count(*) from outbox_events where aggregate_type='GATEWAY_ADMISSION'").Scan(&count); err != nil || count != 0 {
		t.Fatal("expired admission dispatched")
	}
}
