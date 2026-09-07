package httpapi

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sagehou/restfleet/internal/domain"
	"github.com/sagehou/restfleet/internal/persistence/postgres"
	"github.com/sagehou/restfleet/internal/rclone"
	"github.com/sagehou/restfleet/internal/restic"
	"github.com/sagehou/restfleet/internal/security"
	control "github.com/sagehou/restfleet/internal/server"
)

func queueInitialize(t *testing.T, b *testBrowser, id uuid.UUID, key string) Operation {
	t.Helper()
	r := b.request(t, http.MethodPost, "/api/v1/repositories/"+id.String()+"/initialize", nil,
		map[string]string{"X-CSRF-Token": b.cookies[csrfCookieName].Value, "Idempotency-Key": key})
	if r.Code != http.StatusAccepted {
		t.Fatalf("initialize status=%d body=%s", r.Code, r.Body.String())
	}
	var o Operation
	decodeResponse(t, r, &o)
	if o.Type != "REPOSITORY_INITIALIZE" || o.RepositoryId == nil || *o.RepositoryId != id || r.Header().Get("Location") != "/api/v1/operations/"+o.Id.String() {
		t.Fatal("wrong operation contract")
	}
	assertRepositoryMetadata(t, r.Body.String())
	return o
}

func initializerWorker(t *testing.T, store *postgres.Store, run control.RepositoryInitializer) *control.ControlPlane {
	t.Helper()
	c, err := control.NewControlPlane(store, control.Settings{MasterKey: bytes.Repeat([]byte{8}, 32), InitializeRepository: run,
		PasswordParams: security.Argon2Params{Memory: 64, Iterations: 1, Parallelism: 1, SaltLength: 8, KeyLength: 16}})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func provisionInfo() restic.RepositoryInfo {
	return restic.RepositoryInfo{ID: strings.Repeat("a", 64), FormatVersion: 2}
}

func expireInitialize(t *testing.T, pool *pgxpool.Pool, operation uuid.UUID) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `update jobs set lease_expires_at=clock_timestamp()-interval '1 second' where operation_id=$1;
		`, operation)
	if err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(context.Background(), "update repository_leases set expires_at=clock_timestamp()-interval '1 second' where operation_id=$1", operation)
	if err != nil {
		t.Fatal(err)
	}
}

func TestRepositoryInitializeBackendsAndDurableResult(t *testing.T) {
	for _, backend := range []string{"onedrive", "drive", "webdav"} {
		t.Run(backend, func(t *testing.T) {
			store, pool, _, b, _ := setupFleet(t)
			cred := createBackendCredential(t, b, backend)
			host := createTestHost(t, b, "Initialize Host")
			repo := createTestRepository(t, b, host.Id, cred.Id, "Initialize")
			o := queueInitialize(t, b, repo.Id, "same-request")
			if replay := queueInitialize(t, b, repo.Id, "same-request"); replay.Id != o.Id {
				t.Fatal("duplicate identity")
			}
			ctx := context.Background()
			record, err := store.Repository(ctx, repo.Id)
			if err != nil {
				t.Fatal(err)
			}
			var borrowed []byte
			worker := initializerWorker(t, store, func(ctx context.Context, r restic.ProvisionRequest, persist func(context.Context, []byte) error) (restic.RepositoryInfo, error) {
				borrowed = r.Password
				if len(r.Password) != 43 || r.RepositoryID != record.ID || r.GatewayID != record.GatewayID || r.ExpectedID != "" {
					t.Fatal("wrong durable identity or password")
				}
				config, err := rclone.ParseConfig(string(r.Config), r.Remote)
				if err != nil || config.Backend() != backend {
					t.Fatal("wrong cloud backend")
				}
				var accesses int
				if err = pool.QueryRow(ctx, "select count(*) from audit_events where request_id=$1 and action in ('STORAGE_SECRET_ACCESS','REPOSITORY_SECRET_ACCESS')", o.Id).Scan(&accesses); err != nil || accesses != 2 {
					t.Fatal("secret accessed before audit")
				}
				if backend != "webdav" {
					next := bytes.ReplaceAll(r.Config, []byte("storage-refresh-canary"), []byte("initialize-refresh-canary"))
					defer clear(next)
					if err = persist(ctx, next); err != nil {
						return restic.RepositoryInfo{}, err
					}
				}
				return provisionInfo(), nil
			})
			if worked, err := worker.ProcessCredentialJob(ctx, uuid.Must(uuid.NewV7())); err != nil || !worked {
				t.Fatalf("worker: %v %v", worked, err)
			}
			if !bytes.Equal(borrowed, make([]byte, len(borrowed))) {
				t.Fatal("password not cleared")
			}
			got, err := store.Repository(ctx, repo.Id)
			if err != nil || got.Status != "PROVISIONING" || got.InitializedAt == nil || got.ResticID != provisionInfo().ID || got.FormatVersion == nil || *got.FormatVersion != 2 || got.GatewaySecretRef != record.GatewaySecretRef || got.ResticSecretRef != record.ResticSecretRef {
				t.Fatal("initialization lost identity or falsely marked READY")
			}
			if replay := queueInitialize(t, b, repo.Id, "same-request"); replay.Id != o.Id || replay.Status != "SUCCEEDED" {
				t.Fatal("terminal replay changed identity")
			}
			res := b.request(t, http.MethodGet, "/api/v1/repositories/"+repo.Id.String(), nil, nil)
			assertRepositoryMetadata(t, res.Body.String())
			if strings.Contains(res.Body.String(), provisionInfo().ID) || !strings.Contains(res.Body.String(), "initialized_at") {
				t.Fatal("initialization response boundary")
			}
			if b.request(t, http.MethodPost, "/api/v1/repositories/"+repo.Id.String()+"/initialize", nil, map[string]string{"X-CSRF-Token": b.cookies[csrfCookieName].Value, "Idempotency-Key": "new-request"}).Code != http.StatusConflict {
				t.Fatal("initialized repository initialized twice")
			}
			current, err := store.StorageCredential(ctx, cred.Id)
			if err != nil || current.Status != "UNTESTED" || current.LastTestedAt != nil {
				t.Fatal("initialization invented credential test health")
			}
			if backend != "webdav" && (current.SecretRevision != 2 || current.LastRefreshedAt == nil) {
				t.Fatal("refresh not durable")
			}
			var states []string
			if err = pool.QueryRow(ctx, "select array_agg(to_status order by sequence) from operation_events where operation_id=$1", o.Id).Scan(&states); err != nil || strings.Join(states, ",") != "QUEUED,DISPATCHED,ACKNOWLEDGED,RUNNING,SUCCEEDED" {
				t.Fatal("missing transitions")
			}
			var active int
			if err = pool.QueryRow(ctx, "select count(*) from repository_leases where expires_at>clock_timestamp()").Scan(&active); err != nil || active != 0 {
				t.Fatal("completed lease not released")
			}
			if err = store.VerifyAuditChain(ctx); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRepositoryInitializeAuthorizationAndAdmission(t *testing.T) {
	store, pool, handler, b, _ := setupFleet(t)
	cred := createCredential(t, b, "Admission")
	host := createTestHost(t, b, "Admission Host")
	repo := createTestRepository(t, b, host.Id, cred.Id, "Admission")
	path := "/api/v1/repositories/" + repo.Id.String() + "/initialize"
	headers := map[string]string{"X-CSRF-Token": b.cookies[csrfCookieName].Value, "Idempotency-Key": "admission"}
	if newTestBrowser(handler).request(t, http.MethodPost, path, nil, headers).Code != http.StatusUnauthorized {
		t.Fatal("anonymous initialize")
	}
	if b.request(t, http.MethodPost, path, nil, map[string]string{"Idempotency-Key": "csrf"}).Code != http.StatusForbidden {
		t.Fatal("CSRF bypass")
	}
	for _, query := range []string{"?remote=evil", "?force=true"} {
		if b.request(t, http.MethodPost, path+query, nil, headers).Code != http.StatusBadRequest {
			t.Fatal("query accepted")
		}
	}
	if b.request(t, http.MethodPost, path, map[string]string{"password": "forbidden-canary"}, headers).Code != http.StatusBadRequest {
		t.Fatal("body accepted")
	}
	if b.request(t, http.MethodPost, path, nil, map[string]string{"X-CSRF-Token": headers["X-CSRF-Token"]}).Code != http.StatusBadRequest {
		t.Fatal("missing key accepted")
	}
	ctx := context.Background()
	if _, err := pool.Exec(ctx, "update users set role='VIEWER'"); err != nil {
		t.Fatal(err)
	}
	if b.request(t, http.MethodPost, path, nil, headers).Code != http.StatusForbidden {
		t.Fatal("viewer initialized")
	}
	if _, err := pool.Exec(ctx, "update users set role='ADMIN'"); err != nil {
		t.Fatal(err)
	}
	o := queueInitialize(t, b, repo.Id, "admission")
	headers["Idempotency-Key"] = "another"
	if b.request(t, http.MethodPost, path, nil, headers).Code != http.StatusConflict {
		t.Fatal("duplicate active initialization")
	}
	if b.request(t, http.MethodPost, "/api/v1/storage-credentials/"+cred.Id.String()+"/test", nil, headers).Code != http.StatusConflict {
		t.Fatal("concurrent credential test allowed")
	}
	// A backup lease arriving after enqueue still fences worker claim.
	if _, err := pool.Exec(ctx, "insert into repository_leases(repository_id,operation_id,owner,kind,expires_at) values($1,$2,$3,'BACKUP',clock_timestamp()+interval '1 minute')", repo.Id, o.Id, uuid.Must(uuid.NewV7())); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimCredentialJob(ctx, uuid.Must(uuid.NewV7())); !errors.Is(err, domain.ErrRepositoryBusy) {
		t.Fatal("active backup lease ignored")
	}
	var status string
	if err := pool.QueryRow(ctx, "select status from operations where id=$1", o.Id).Scan(&status); err != nil || status != "QUEUED" {
		t.Fatal("blocked claim partially committed")
	}
}

func TestRepositoryInitializeShutdownAndFencing(t *testing.T) {
	store, pool, _, b, _ := setupFleet(t)
	cred := createCredential(t, b, "Recover")
	host := createTestHost(t, b, "Recover Host")
	repo := createTestRepository(t, b, host.Id, cred.Id, "Recover")
	o := queueInitialize(t, b, repo.Id, "recover")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var original []byte
	worker := initializerWorker(t, store, func(ctx context.Context, r restic.ProvisionRequest, persist func(context.Context, []byte) error) (restic.RepositoryInfo, error) {
		original = append([]byte(nil), r.Password...)
		next := bytes.ReplaceAll(r.Config, []byte("storage-refresh-canary"), []byte("recovered-refresh-canary"))
		defer clear(next)
		if err := persist(ctx, next); err != nil {
			return restic.RepositoryInfo{}, err
		}
		cancel()
		return restic.RepositoryInfo{}, ctx.Err()
	})
	defer func() { clear(original) }()
	owner := uuid.Must(uuid.NewV7())
	if _, err := worker.ProcessCredentialJob(ctx, owner); !errors.Is(err, context.Canceled) {
		t.Fatal("shutdown not propagated")
	}
	ctx = context.Background()
	got, err := store.Operation(ctx, o.Id)
	if err != nil || got.Status != "RUNNING" {
		t.Fatal("shutdown falsified terminal state")
	}
	var jobID uuid.UUID
	if err = pool.QueryRow(ctx, "select id from jobs where operation_id=$1", o.Id).Scan(&jobID); err != nil {
		t.Fatal(err)
	}
	expireInitialize(t, pool, o.Id)
	if err = store.RenewCredentialJob(ctx, jobID, owner); !errors.Is(err, domain.ErrJobLeaseLost) {
		t.Fatal("expired lease renewed")
	}
	if err = store.CompleteRepositoryJob(ctx, jobID, owner, "", provisionInfo().ID, 2); !errors.Is(err, domain.ErrJobLeaseLost) {
		t.Fatal("expired owner completed")
	}
	restarted := initializerWorker(t, store, func(_ context.Context, r restic.ProvisionRequest, _ func(context.Context, []byte) error) (restic.RepositoryInfo, error) {
		if !bytes.Equal(original, r.Password) || r.RepositoryID != repo.Id || !bytes.Contains(r.Config, []byte("recovered-refresh-canary")) {
			t.Fatal("restart regenerated password or lost token")
		}
		return provisionInfo(), nil
	})
	if _, err = restarted.ProcessCredentialJob(ctx, uuid.Must(uuid.NewV7())); err != nil {
		t.Fatal(err)
	}
	got, err = store.Operation(ctx, o.Id)
	if err != nil || got.Status != "SUCCEEDED" || got.Attempt != 2 {
		t.Fatal("restart did not recover")
	}
	if _, err = store.RefreshCredentialJob(ctx, jobID, owner, 2, domain.SecretEnvelope{}); !errors.Is(err, domain.ErrJobLeaseLost) {
		t.Fatal("old owner refreshed")
	}
}

func TestRepositoryInitializeFailuresNeverPromoteOrLeak(t *testing.T) {
	for _, tc := range []struct {
		code string
		err  error
	}{
		{"REPOSITORY_LOCKED", restic.ErrRepositoryLocked}, {"PASSWORD_REJECTED", restic.ErrWrongPassword},
		{"REPOSITORY_MISMATCH", restic.ErrRepositoryMatch}, {"REPOSITORY_NOT_EMPTY", restic.ErrRepositoryInUse},
		{"INITIALIZE_TIMED_OUT", context.DeadlineExceeded}, {"INITIALIZE_FAILED", errors.New("storage-access-canary subprocess failure")},
		{"CONFIG_UNSAFE", rclone.ErrConfigChanged}, {"REFRESH_FAILED", rclone.ErrRefreshPersist},
	} {
		t.Run(tc.code, func(t *testing.T) {
			store, _, _, b, _ := setupFleet(t)
			cred := createCredential(t, b, "Failure")
			host := createTestHost(t, b, "Failure Host")
			repo := createTestRepository(t, b, host.Id, cred.Id, "Failure")
			o := queueInitialize(t, b, repo.Id, "failure")
			worker := initializerWorker(t, store, func(context.Context, restic.ProvisionRequest, func(context.Context, []byte) error) (restic.RepositoryInfo, error) {
				return provisionInfo(), tc.err
			})
			ctx := context.Background()
			if _, err := worker.ProcessCredentialJob(ctx, uuid.Must(uuid.NewV7())); err != nil {
				t.Fatal(err)
			}
			got, err := store.Operation(ctx, o.Id)
			if err != nil || got.ErrorCode != tc.code || got.FinishedAt == nil {
				t.Fatalf("result=%+v err=%v", got, err)
			}
			r, err := store.Repository(ctx, repo.Id)
			if err != nil || r.InitializedAt != nil || r.FormatVersion != nil || r.Status != "PROVISIONING" {
				t.Fatal("failure promoted repository")
			}
			assertNoStorageSecret(t, b.request(t, http.MethodGet, "/api/v1/operations/"+o.Id.String(), nil, nil).Body.String())
			if retry := queueInitialize(t, b, repo.Id, "retry"); retry.Id == o.Id {
				t.Fatal("retry reopened terminal operation")
			}
		})
	}
}

func TestRepositoryInitializeAuditFailureRollsBack(t *testing.T) {
	for _, stage := range []string{"REPOSITORY_INITIALIZE", "REPOSITORY_SECRET_ACCESS", "REPOSITORY_INITIALIZE_RESULT"} {
		t.Run(stage, func(t *testing.T) {
			store, pool, _, b, _ := setupFleet(t)
			cred := createCredential(t, b, "Audit")
			host := createTestHost(t, b, "Audit Host")
			repo := createTestRepository(t, b, host.Id, cred.Id, "Audit")
			if stage == "REPOSITORY_INITIALIZE" {
				rejectCredentialAudit(t, pool, stage)
				r := b.request(t, http.MethodPost, "/api/v1/repositories/"+repo.Id.String()+"/initialize", nil, map[string]string{"X-CSRF-Token": b.cookies[csrfCookieName].Value, "Idempotency-Key": "audit"})
				if r.Code != http.StatusServiceUnavailable {
					t.Fatal("enqueue accepted without audit")
				}
				var count int
				if err := pool.QueryRow(context.Background(), "select count(*) from jobs").Scan(&count); err != nil || count != 0 {
					t.Fatal("orphan job")
				}
				return
			}
			o := queueInitialize(t, b, repo.Id, "audit")
			rejectCredentialAudit(t, pool, stage)
			called := false
			worker := initializerWorker(t, store, func(context.Context, restic.ProvisionRequest, func(context.Context, []byte) error) (restic.RepositoryInfo, error) {
				called = true
				return provisionInfo(), nil
			})
			if _, err := worker.ProcessCredentialJob(context.Background(), uuid.Must(uuid.NewV7())); err == nil {
				t.Fatal("audit failure ignored")
			}
			if stage == "REPOSITORY_SECRET_ACCESS" && called {
				t.Fatal("executed without secret access audit")
			}
			got, err := store.Operation(context.Background(), o.Id)
			if err != nil || got.FinishedAt != nil {
				t.Fatal("audit failure committed terminal state")
			}
			r, err := store.Repository(context.Background(), repo.Id)
			if err != nil || r.InitializedAt != nil {
				t.Fatal("audit failure committed metadata")
			}
		})
	}
}

func TestRepositoryInitializationRejectsInFlightChanges(t *testing.T) {
	for _, mode := range []string{"host-disabled", "credential-disabled", "password-substitution", "invalid-result", "lease-lost"} {
		t.Run(mode, func(t *testing.T) {
			store, pool, _, b, _ := setupFleet(t)
			cred := createCredential(t, b, "Changed")
			host := createTestHost(t, b, "Changed Host")
			repo := createTestRepository(t, b, host.Id, cred.Id, "Changed")
			o := queueInitialize(t, b, repo.Id, "changed")
			ctx := context.Background()
			if mode == "password-substitution" {
				if _, err := pool.Exec(ctx, "update secrets set aad=decode('00','hex') where id=(select restic_secret_ref from repositories where id=$1)", repo.Id); err != nil {
					t.Fatal(err)
				}
			}
			called := false
			worker := initializerWorker(t, store, func(ctx context.Context, _ restic.ProvisionRequest, _ func(context.Context, []byte) error) (restic.RepositoryInfo, error) {
				called = true
				var err error
				switch mode {
				case "host-disabled":
					_, err = pool.Exec(ctx, "update hosts set status='DISABLED' where id=$1", host.Id)
				case "credential-disabled":
					_, err = pool.Exec(ctx, "update storage_credentials set status='DISABLED' where id=$1", cred.Id)
				case "invalid-result":
					return restic.RepositoryInfo{ID: "not-a-native-id", FormatVersion: 2}, nil
				case "lease-lost":
					_, err = pool.Exec(ctx, "update repository_leases set expires_at=clock_timestamp()-interval '1 second' where repository_id=$1", repo.Id)
				}
				if err != nil {
					t.Fatal(err)
				}
				return provisionInfo(), nil
			})
			_, err := worker.ProcessCredentialJob(ctx, uuid.Must(uuid.NewV7()))
			if mode == "lease-lost" {
				if !errors.Is(err, domain.ErrJobLeaseLost) {
					t.Fatal("lost repository fence ignored")
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if mode == "password-substitution" && called {
				t.Fatal("substituted password passed to Restic")
			}
			r, err := store.Repository(ctx, repo.Id)
			if err != nil || r.InitializedAt != nil {
				t.Fatal("unsafe result committed")
			}
			op, err := store.Operation(ctx, o.Id)
			if err != nil {
				t.Fatal(err)
			}
			want := map[string]string{"host-disabled": "REPOSITORY_UNAVAILABLE", "credential-disabled": "CREDENTIAL_DISABLED", "password-substitution": "SECRET_UNAVAILABLE", "invalid-result": "INITIALIZE_FAILED", "lease-lost": ""}[mode]
			if op.ErrorCode != want {
				t.Fatalf("error code=%s want=%s", op.ErrorCode, want)
			}
		})
	}
}

func TestRepositoryCompletionRechecksBothLeasesAfterAudit(t *testing.T) {
	for _, fence := range []string{"job", "repository"} {
		t.Run(fence, func(t *testing.T) {
			store, pool, _, b, _ := setupFleet(t)
			cred := createCredential(t, b, "Late fence")
			host := createTestHost(t, b, "Late fence Host")
			repo := createTestRepository(t, b, host.Id, cred.Id, "Late fence")
			o := queueInitialize(t, b, repo.Id, "late-fence")
			ctx := context.Background()
			owner := uuid.Must(uuid.NewV7())
			job, err := store.ClaimCredentialJob(ctx, owner)
			if err != nil {
				t.Fatal(err)
			}
			expire := "update jobs set lease_expires_at=clock_timestamp()-interval '1 second' where operation_id=NEW.request_id;"
			if fence == "repository" {
				expire = "update repository_leases set expires_at=clock_timestamp()-interval '1 second' where operation_id=NEW.request_id;"
			}
			_, err = pool.Exec(ctx, `create function expire_initialize_audit() returns trigger language plpgsql as $$
				begin if NEW.action='REPOSITORY_INITIALIZE_RESULT' then `+expire+`
				end if; return NEW; end $$;
				create trigger expire_initialize_audit before insert on audit_events
				for each row execute function expire_initialize_audit();`)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				_, _ = pool.Exec(ctx, "drop trigger if exists expire_initialize_audit on audit_events; drop function if exists expire_initialize_audit()")
			})
			if err = store.CompleteRepositoryJob(ctx, job.ID, owner, "", provisionInfo().ID, 2); !errors.Is(err, domain.ErrJobLeaseLost) {
				t.Fatal("late fence ignored")
			}
			r, err := store.Repository(ctx, repo.Id)
			if err != nil || r.InitializedAt != nil || r.FormatVersion != nil {
				t.Fatal("late fence committed repository")
			}
			got, err := store.Operation(ctx, o.Id)
			if err != nil || got.Status != "RUNNING" {
				t.Fatal("late fence committed operation")
			}
		})
	}
}
