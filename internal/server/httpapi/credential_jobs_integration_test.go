package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sagehou/restfleet/internal/domain"
	"github.com/sagehou/restfleet/internal/persistence/postgres"
	"github.com/sagehou/restfleet/internal/rclone"
	"github.com/sagehou/restfleet/internal/security"
	control "github.com/sagehou/restfleet/internal/server"
)

func queueCredentialTest(t *testing.T, b *testBrowser, id uuid.UUID, key string) Operation {
	t.Helper()
	r := b.request(t, http.MethodPost, "/api/v1/storage-credentials/"+id.String()+"/test", nil, map[string]string{
		"X-CSRF-Token": b.cookies[csrfCookieName].Value, "Idempotency-Key": key,
	})
	if r.Code != http.StatusAccepted {
		t.Fatalf("test enqueue = %d", r.Code)
	}
	var o Operation
	decodeResponse(t, r, &o)
	if r.Header().Get("Location") != "/api/v1/operations/"+o.Id.String() || r.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("missing operation response headers")
	}
	assertNoStorageSecret(t, r.Body.String())
	return o
}

func credentialWorker(t *testing.T, store *postgres.Store, run control.CredentialTestRunner) *control.ControlPlane {
	t.Helper()
	c, err := control.NewControlPlane(store, control.Settings{MasterKey: bytes.Repeat([]byte{8}, 32),
		RunCredentialTest: run, PasswordParams: security.Argon2Params{Memory: 64, Iterations: 1, Parallelism: 1, SaltLength: 8, KeyLength: 16}})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func expireCredentialJob(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), "update jobs set lease_expires_at=clock_timestamp()-interval '1 second' where id=$1", id); err != nil {
		t.Fatal(err)
	}
}

func TestCredentialOperationRefreshAndRestart(t *testing.T) {
	store, pool, _, b, _ := setupFleet(t)
	c := createCredential(t, b, "Async")
	o := queueCredentialTest(t, b, c.Id, "same-request")
	replay := queueCredentialTest(t, b, c.Id, "same-request")
	if replay.Id != o.Id || string(o.Status) != "QUEUED" {
		t.Fatal("request replay created a new operation")
	}
	busy := b.request(t, http.MethodPost, "/api/v1/storage-credentials/"+c.Id.String()+"/test", nil, map[string]string{
		"X-CSRF-Token": b.cookies[csrfCookieName].Value, "Idempotency-Key": "another-request",
	})
	if busy.Code != http.StatusConflict {
		t.Fatal("overlapping test accepted")
	}
	runner := func(ctx context.Context, raw []byte, remote string, persist func(context.Context, []byte) error) error {
		if remote != "encrypted" || !bytes.Contains(raw, []byte("storage-refresh-canary")) {
			t.Fatal("wrong runtime material")
		}
		for _, token := range []string{"storage-new-canary", "storage-final-canary"} {
			next := bytes.ReplaceAll(raw, []byte("storage-refresh-canary"), []byte(token))
			err := persist(ctx, next)
			clear(next)
			if err != nil {
				return err
			}
		}
		return nil
	}
	// A fresh ControlPlane instance consumes the persisted queue after enqueue.
	worker := credentialWorker(t, store, runner)
	if worked, err := worker.ProcessCredentialJob(context.Background(), uuid.Must(uuid.NewV7())); err != nil || !worked {
		t.Fatalf("worker = %v, %v", worked, err)
	}
	got, err := store.Operation(context.Background(), o.Id)
	if err != nil || got.Status != "SUCCEEDED" || got.SecretRevision != 3 || got.FinishedAt == nil {
		t.Fatalf("operation = %+v, %v", got, err)
	}
	current, err := store.StorageCredential(context.Background(), c.Id)
	if err != nil || current.Status != "HEALTHY" || current.SecretRevision != 3 || current.LastRefreshedAt == nil || current.LastTestedAt == nil || current.LastTestResult != "SUCCEEDED" {
		t.Fatalf("credential metadata = %+v, %v", current, err)
	}
	e, err := store.StorageCredentialSecret(context.Background(), current.SecretRef)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := security.OpenEnvelope(bytes.Repeat([]byte{8}, 32), security.Envelope{Ciphertext: e.Ciphertext, Nonce: e.Nonce, WrappedDataKey: e.WrappedDataKey, WrapNonce: e.WrapNonce, AAD: e.AAD})
	if err != nil {
		t.Fatal(err)
	}
	defer clear(raw)
	if !bytes.Contains(raw, []byte("storage-final-canary")) || bytes.Contains(e.Ciphertext, []byte("storage-final-canary")) {
		t.Fatal("refresh not encrypted and durable")
	}
	var states []string
	if err := pool.QueryRow(context.Background(), "select array_agg(to_status order by sequence) from operation_events where operation_id=$1", o.Id).Scan(&states); err != nil {
		t.Fatal(err)
	}
	if strings.Join(states, ",") != "QUEUED,DISPATCHED,ACKNOWLEDGED,RUNNING,SUCCEEDED" {
		t.Fatalf("state events = %v", states)
	}
	var outbox int
	if err := pool.QueryRow(context.Background(), "select count(*) from outbox_events where aggregate_id=$1", o.Id).Scan(&outbox); err != nil || outbox != 5 {
		t.Fatal("transactional outbox incomplete")
	}
	if err := store.VerifyAuditChain(context.Background()); err != nil {
		t.Fatal(err)
	}
	if replay := queueCredentialTest(t, b, c.Id, "same-request"); replay.Id != o.Id || string(replay.Status) != "SUCCEEDED" {
		t.Fatal("terminal request replay changed identity")
	}
	response := b.request(t, http.MethodGet, "/api/v1/operations/"+o.Id.String(), nil, nil)
	if response.Code != http.StatusOK {
		t.Fatal("operation read failed")
	}
	assertNoStorageSecret(t, response.Body.String())
	// Metadata must not expose ciphertext or runtime credentials.
	assertNoStorageSecret(t, b.request(t, http.MethodGet, "/api/v1/storage-credentials/"+c.Id.String(), nil, nil).Body.String())
	if _, err := pool.Exec(context.Background(), "update users set role='VIEWER'"); err != nil {
		t.Fatal(err)
	}
	if b.request(t, http.MethodGet, "/api/v1/operations/"+o.Id.String(), nil, nil).Code != http.StatusOK {
		t.Fatal("viewer cannot read operation")
	}
	if b.request(t, http.MethodPost, "/api/v1/storage-credentials/"+c.Id.String()+"/test", nil, map[string]string{"X-CSRF-Token": b.cookies[csrfCookieName].Value, "Idempotency-Key": "viewer"}).Code != http.StatusForbidden {
		t.Fatal("viewer queued a test")
	}
}

func TestCredentialOperationConcurrentIdempotency(t *testing.T) {
	_, pool, handler, b, _ := setupFleet(t)
	c := createCredential(t, b, "Concurrent queue")
	type result struct {
		code int
		body string
	}
	results := make(chan result, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := httptest.NewRequest(http.MethodPost, "/api/v1/storage-credentials/"+c.Id.String()+"/test", nil)
			r.Header.Set("X-CSRF-Token", b.cookies[csrfCookieName].Value)
			r.Header.Set("Idempotency-Key", "duplicate")
			for _, cookie := range b.cookies {
				r.AddCookie(cookie)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, r)
			results <- result{response.Code, response.Body.String()}
		}()
	}
	wg.Wait()
	close(results)
	var id uuid.UUID
	for r := range results {
		if r.code != http.StatusAccepted {
			t.Fatalf("concurrent enqueue = %d", r.code)
		}
		var o Operation
		if json.Unmarshal([]byte(r.body), &o) != nil {
			t.Fatal("invalid operation response")
		}
		if id != uuid.Nil && id != o.Id {
			t.Fatal("duplicate operation identities")
		}
		id = o.Id
	}
	var count int
	if err := pool.QueryRow(context.Background(), "select count(*) from jobs").Scan(&count); err != nil || count != 1 {
		t.Fatal("duplicate jobs")
	}
	if err := pool.QueryRow(context.Background(), "select count(*) from idempotency_records where octet_length(key_hash)=32 and expires_at>=created_at+interval '24 hours'").Scan(&count); err != nil || count != 1 {
		t.Fatal("idempotency retention/hash invalid")
	}
	// A conflicting stored body hash must not return the old resource.
	if _, err := pool.Exec(context.Background(), "update idempotency_records set request_hash=decode(repeat('01',32),'hex')"); err != nil {
		t.Fatal(err)
	}
	r := b.request(t, http.MethodPost, "/api/v1/storage-credentials/"+c.Id.String()+"/test", nil, map[string]string{"X-CSRF-Token": b.cookies[csrfCookieName].Value, "Idempotency-Key": "duplicate"})
	if r.Code != http.StatusConflict || !strings.Contains(r.Body.String(), "IDEMPOTENCY_KEY_REUSED") {
		t.Fatal("idempotency conflict not enforced")
	}
}

func TestCredentialJobLeaseRecoveryAndFencing(t *testing.T) {
	store, pool, _, b, _ := setupFleet(t)
	c := createCredential(t, b, "Lease")
	o := queueCredentialTest(t, b, c.Id, "lease")
	ctx := context.Background()
	owner := uuid.Must(uuid.NewV7())
	job, err := store.ClaimCredentialJob(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimCredentialJob(ctx, uuid.Must(uuid.NewV7())); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("leased job delivered twice")
	}
	if err := store.RenewCredentialJob(ctx, job.ID, owner); err != nil {
		t.Fatal(err)
	}
	expireCredentialJob(t, pool, job.ID)
	if err := store.RenewCredentialJob(ctx, job.ID, owner); !errors.Is(err, domain.ErrJobLeaseLost) {
		t.Fatal("expired lease resurrected")
	}
	if err := store.CompleteCredentialJob(ctx, job.ID, owner, ""); !errors.Is(err, domain.ErrJobLeaseLost) {
		t.Fatal("expired worker completed")
	}
	nextOwner := uuid.Must(uuid.NewV7())
	next, err := store.ClaimCredentialJob(ctx, nextOwner)
	if err != nil || next.Operation.ID != o.Id || next.Operation.Attempt != 2 || next.Operation.Status != "RUNNING" {
		t.Fatalf("recovery = %+v, %v", next, err)
	}
	if _, err := store.RefreshCredentialJob(ctx, job.ID, owner, 1, domain.SecretEnvelope{}); !errors.Is(err, domain.ErrJobLeaseLost) {
		t.Fatal("old worker refreshed after takeover")
	}
	if err := store.CompleteCredentialJob(ctx, job.ID, owner, ""); !errors.Is(err, domain.ErrJobLeaseLost) {
		t.Fatal("old worker completed after takeover")
	}
	if err := store.CompleteCredentialJob(ctx, job.ID, nextOwner, ""); err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteCredentialJob(ctx, job.ID, nextOwner, ""); !errors.Is(err, domain.ErrJobLeaseLost) {
		t.Fatal("terminal operation reopened")
	}
	_ = queueCredentialTest(t, b, c.Id, "exhaust")
	for range 3 {
		j, err := store.ClaimCredentialJob(ctx, uuid.Must(uuid.NewV7()))
		if err != nil {
			t.Fatal(err)
		}
		expireCredentialJob(t, pool, j.ID)
	}
	if _, err := store.ClaimCredentialJob(ctx, uuid.Must(uuid.NewV7())); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("exhausted job reclaimed")
	}
	var status, code string
	if err := pool.QueryRow(ctx, "select status,error_code from operations where id<>$1", o.Id).Scan(&status, &code); err != nil || status != "TIMED_OUT" || code != "WORKER_LOST" {
		t.Fatalf("exhaustion = %s %s %v", status, code, err)
	}
}

func TestCredentialTestInvalidatesStaleResults(t *testing.T) {
	for _, mode := range []string{"replace", "disable", "replace-refresh", "disable-refresh", "unsafe-refresh", "provider-error", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			store, _, _, b, _ := setupFleet(t)
			c := createCredential(t, b, "In flight")
			o := queueCredentialTest(t, b, c.Id, "test")
			worker := credentialWorker(t, store, func(ctx context.Context, raw []byte, _ string, persist func(context.Context, []byte) error) error {
				if mode == "provider-error" {
					return errors.New("storage-access-canary provider error")
				}
				if mode == "timeout" {
					return context.DeadlineExceeded
				}
				if mode == "unsafe-refresh" {
					next := bytes.ReplaceAll(raw, []byte("cloud:backups"), []byte("cloud:elsewhere"))
					if err := persist(ctx, next); err == nil {
						t.Fatal("runtime changed storage target")
					}
					return rclone.ErrConfigChanged
				}
				current, err := store.StorageCredential(ctx, c.Id)
				if err != nil {
					t.Fatal(err)
				}
				path := "/api/v1/storage-credentials/" + c.Id.String() + "/"
				var body any
				if strings.HasPrefix(mode, "replace") {
					path += "replace-secret"
					body = StorageCredentialReplace{RcloneConfig: credentialConfig(strings.ReplaceAll(credentialFixture(), "storage-refresh-canary", "storage-new-canary"))}
				} else {
					path += "disable"
				}
				r := b.request(t, http.MethodPost, path, body, map[string]string{"X-CSRF-Token": b.cookies[csrfCookieName].Value, "If-Match": fmt.Sprintf("%q", fmt.Sprint(current.Revision))})
				if r.Code != http.StatusOK {
					t.Fatalf("concurrent credential change = %d", r.Code)
				}
				if strings.HasSuffix(mode, "-refresh") {
					next := bytes.ReplaceAll(raw, []byte("storage-refresh-canary"), []byte("stale-worker-token"))
					if err := persist(ctx, next); !errors.Is(err, domain.ErrRevisionConflict) {
						t.Fatalf("stale CAS = %v", err)
					}
					return rclone.ErrRefreshPersist
				}
				return nil
			})
			if _, err := worker.ProcessCredentialJob(context.Background(), uuid.Must(uuid.NewV7())); err != nil {
				t.Fatal(err)
			}
			got, err := store.Operation(context.Background(), o.Id)
			if err != nil {
				t.Fatal(err)
			}
			expected := "CREDENTIAL_CHANGED"
			switch {
			case strings.HasPrefix(mode, "disable"):
				expected = "CREDENTIAL_DISABLED"
			case mode == "provider-error":
				expected = "CONNECTION_FAILED"
			case mode == "unsafe-refresh":
				expected = "CONFIG_UNSAFE"
			case mode == "timeout":
				expected = "TEST_TIMED_OUT"
			}
			if got.ErrorCode != expected || got.FinishedAt == nil {
				t.Fatalf("stale result = %+v", got)
			}
			current, err := store.StorageCredential(context.Background(), c.Id)
			if err != nil {
				t.Fatal(err)
			}
			if current.Status == "HEALTHY" {
				t.Fatal("stale/failed result promoted credential")
			}
			assertNoStorageSecret(t, b.request(t, http.MethodGet, "/api/v1/operations/"+o.Id.String(), nil, nil).Body.String())
		})
	}
}

func TestCredentialOperationAuthorizationAndBody(t *testing.T) {
	_, _, handler, b, _ := setupFleet(t)
	c := createCredential(t, b, "Auth")
	path := "/api/v1/storage-credentials/" + c.Id.String() + "/test"
	headers := map[string]string{"X-CSRF-Token": b.cookies[csrfCookieName].Value, "Idempotency-Key": "valid"}
	if newTestBrowser(handler).request(t, http.MethodPost, path, nil, headers).Code != http.StatusUnauthorized {
		t.Fatal("anonymous enqueue accepted")
	}
	if b.request(t, http.MethodPost, path, nil, map[string]string{"X-CSRF-Token": "wrong", "Idempotency-Key": "valid"}).Code != http.StatusForbidden {
		t.Fatal("CSRF bypass")
	}
	if b.request(t, http.MethodPost, path, map[string]string{"secret": "storage-access-canary"}, headers).Code != http.StatusBadRequest {
		t.Fatal("body accepted")
	}
	if b.request(t, http.MethodPost, path+"?remote=evil", nil, headers).Code != http.StatusBadRequest {
		t.Fatal("query accepted")
	}
	if b.request(t, http.MethodPost, path, nil, map[string]string{"X-CSRF-Token": b.cookies[csrfCookieName].Value}).Code != http.StatusBadRequest {
		t.Fatal("missing idempotency key accepted")
	}
	o := queueCredentialTest(t, b, c.Id, "valid")
	if newTestBrowser(handler).request(t, http.MethodGet, "/api/v1/operations/"+o.Id.String(), nil, nil).Code != http.StatusUnauthorized {
		t.Fatal("anonymous operation read")
	}
}

func rejectCredentialAudit(t *testing.T, pool *pgxpool.Pool, action string) {
	t.Helper()
	// action is a fixed test case constant, never user input.
	if _, err := pool.Exec(context.Background(), `create function reject_credential_job_audit() returns trigger language plpgsql as $$
		begin if NEW.action = '`+action+`' then raise exception 'storage-access-canary'; end if; return NEW; end $$;
		create trigger reject_credential_job_audit before insert on audit_events
		for each row execute function reject_credential_job_audit();`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "drop trigger if exists reject_credential_job_audit on audit_events; drop function if exists reject_credential_job_audit()")
	})
}

func TestCredentialJobAuditRollback(t *testing.T) {
	for _, stage := range []string{"enqueue", "refresh", "complete"} {
		t.Run(stage, func(t *testing.T) {
			store, pool, _, b, _ := setupFleet(t)
			c := createCredential(t, b, "Audit "+stage)
			ctx := context.Background()
			if stage == "enqueue" {
				rejectCredentialAudit(t, pool, "STORAGE_CREDENTIAL_TEST")
				r := b.request(t, http.MethodPost, "/api/v1/storage-credentials/"+c.Id.String()+"/test", nil, map[string]string{"X-CSRF-Token": b.cookies[csrfCookieName].Value, "Idempotency-Key": "audit"})
				if r.Code != http.StatusInternalServerError {
					t.Fatal("enqueue survived audit failure")
				}
				assertNoStorageSecret(t, r.Body.String())
				var count int
				if err := pool.QueryRow(ctx, "select count(*) from operations").Scan(&count); err != nil || count != 0 {
					t.Fatal("queue transaction partially committed")
				}
				return
			}
			o := queueCredentialTest(t, b, c.Id, "audit")
			action := "STORAGE_CREDENTIAL_REFRESH"
			if stage == "complete" {
				action = "STORAGE_CREDENTIAL_TEST_RESULT"
			}
			rejectCredentialAudit(t, pool, action)
			worker := credentialWorker(t, store, func(ctx context.Context, raw []byte, _ string, persist func(context.Context, []byte) error) error {
				if stage == "complete" {
					return nil
				}
				next := bytes.ReplaceAll(raw, []byte("storage-refresh-canary"), []byte("storage-new-canary"))
				if err := persist(ctx, next); err == nil {
					t.Fatal("refresh survived audit failure")
				}
				return rclone.ErrRefreshPersist
			})
			owner := uuid.Must(uuid.NewV7())
			_, err := worker.ProcessCredentialJob(ctx, owner)
			if stage == "complete" && err == nil {
				t.Fatal("completion survived audit failure")
			}
			if stage == "refresh" && err != nil {
				t.Fatal(err)
			}
			current, err := store.StorageCredential(ctx, c.Id)
			if err != nil {
				t.Fatal(err)
			}
			if current.SecretRevision != 1 || current.LastRefreshedAt != nil {
				t.Fatal("failed refresh committed")
			}
			var versions int
			if err := pool.QueryRow(ctx, "select count(*) from storage_credential_revisions where credential_id=$1", c.Id).Scan(&versions); err != nil || versions != 1 {
				t.Fatal("orphan encrypted revision committed")
			}
			got, err := store.Operation(ctx, o.Id)
			if err != nil {
				t.Fatal(err)
			}
			if stage == "complete" && (got.Status != "RUNNING" || got.FinishedAt != nil) {
				t.Fatal("failed audit committed terminal state")
			}
			if stage == "refresh" && got.ErrorCode != "REFRESH_FAILED" {
				t.Fatalf("refresh result = %+v", got)
			}
		})
	}
}

func TestCredentialWorkerShutdownLeavesRecoverableLease(t *testing.T) {
	store, pool, _, b, _ := setupFleet(t)
	c := createCredential(t, b, "Shutdown")
	o := queueCredentialTest(t, b, c.Id, "shutdown")
	ctx, cancel := context.WithCancel(context.Background())
	worker := credentialWorker(t, store, func(ctx context.Context, raw []byte, _ string, persist func(context.Context, []byte) error) error {
		next := bytes.ReplaceAll(raw, []byte("storage-refresh-canary"), []byte("storage-new-canary"))
		if err := persist(ctx, next); err != nil {
			t.Fatal(err)
		}
		clear(next)
		cancel()
		<-ctx.Done()
		return ctx.Err()
	})
	if _, err := worker.ProcessCredentialJob(ctx, uuid.Must(uuid.NewV7())); !errors.Is(err, context.Canceled) {
		t.Fatal("shutdown not propagated")
	}
	got, err := store.Operation(context.Background(), o.Id)
	if err != nil || got.Status != "RUNNING" {
		t.Fatal("shutdown falsely reported terminal state")
	}
	var jobID uuid.UUID
	if err := pool.QueryRow(context.Background(), "select id from jobs where operation_id=$1", o.Id).Scan(&jobID); err != nil {
		t.Fatal(err)
	}
	expireCredentialJob(t, pool, jobID)
	restarted := credentialWorker(t, store, func(_ context.Context, raw []byte, _ string, _ func(context.Context, []byte) error) error {
		if !bytes.Contains(raw, []byte("storage-new-canary")) {
			t.Fatal("restart lost refreshed token")
		}
		return nil
	})
	if _, err := restarted.ProcessCredentialJob(context.Background(), uuid.Must(uuid.NewV7())); err != nil {
		t.Fatal(err)
	}
	got, err = store.Operation(context.Background(), o.Id)
	if err != nil || got.Status != "SUCCEEDED" || got.Attempt != 2 {
		t.Fatal("restart did not recover job")
	}
}

func TestCredentialRuntimeToEncryptedDatabase(t *testing.T) {
	store, _, _, b, _ := setupFleet(t)
	c := createCredential(t, b, "Runtime integration")
	o := queueCredentialTest(t, b, c.Id, "runtime")
	root, err := os.MkdirTemp("/dev/shm", "restfleet-worker-test-")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(root) }()
	binary := filepath.Join(t.TempDir(), "rclone")
	// Fake executable only; the production adapter always uses fixed argv APIs.
	script := "#!/bin/sh\nsed -i 's/storage-refresh-canary/storage-new-canary/g' \"$5\"\nprintf '%s' '{\"IsDir\":true}'\n"
	if err := os.WriteFile(binary, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	runtime, err := rclone.NewRuntime(root, binary)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = runtime.Close() }()
	worker := credentialWorker(t, store, runtime.Test)
	if _, err := worker.ProcessCredentialJob(context.Background(), uuid.Must(uuid.NewV7())); err != nil {
		t.Fatal(err)
	}
	got, err := store.Operation(context.Background(), o.Id)
	if err != nil || got.Status != "SUCCEEDED" || got.SecretRevision != 2 {
		t.Fatalf("runtime result = %+v, %v", got, err)
	}
	if _, offset := got.CreatedAt.Zone(); offset != 0 {
		t.Fatal("operation timestamp is not UTC")
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 1 || entries[0].Name() != ".lock" {
		t.Fatal("materialized plaintext survived")
	}
}
