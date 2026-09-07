package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sagehou/restfleet/internal/domain"
	"github.com/sagehou/restfleet/internal/security"
)

func createTestRepository(t *testing.T, b *testBrowser, host, credential uuid.UUID, name string) Repository {
	t.Helper()
	res := b.request(t, http.MethodPost, "/api/v1/repositories", RepositoryCreate{Name: name, HostId: host, StorageCredentialId: credential},
		map[string]string{"X-CSRF-Token": b.cookies[csrfCookieName].Value})
	if res.Code != http.StatusCreated {
		t.Fatalf("repository create status=%d body=%s", res.Code, res.Body.String())
	}
	var repo Repository
	decodeResponse(t, res, &repo)
	if res.Header().Get("Location") != "/api/v1/repositories/"+repo.Id.String() || res.Header().Get("ETag") != "\"1\"" || res.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("repository headers missing")
	}
	assertRepositoryMetadata(t, res.Body.String())
	return repo
}

func assertRepositoryMetadata(t *testing.T, raw string) {
	t.Helper()
	assertNoStorageSecret(t, raw)
	for _, forbidden := range []string{"backend_path", "gateway_username", "gateway_password", "restic_password", "wrapped_data_key", "ciphertext"} {
		if strings.Contains(raw, forbidden) {
			t.Fatal("private repository material reached API")
		}
	}
}

func TestRepositoryLifecycleIsolationAndMetadata(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Microsecond)
	store, pool, handler, logs := setupIntegration(t, "bootstrap-test-token", &now)
	b := newTestBrowser(handler)
	if bootstrapAdmin(t, b, "bootstrap-test-token", "a-strong-test-password").Code != http.StatusCreated {
		t.Fatal("bootstrap failed")
	}
	credential := createCredential(t, b, "Shared central credential")
	a := createTestHost(t, b, "Host A")
	other := createTestHost(t, b, "Host B")
	first := createTestRepository(t, b, a.Id, credential.Id, "Archive A")
	second := createTestRepository(t, b, other.Id, credential.Id, "Archive B")
	ctx := context.Background()
	seen := map[string]bool{}
	gateways := map[uuid.UUID]bool{}
	for _, repo := range []Repository{first, second} {
		if repo.Id.Version() != 7 || repo.Status != "PROVISIONING" || repo.FormatVersion != nil ||
			repo.GatewaySecretRevision != 1 || repo.ResticSecretRevision != 1 {
			t.Fatal("unverified repository marked ready")
		}
		record, err := store.Repository(ctx, repo.Id)
		if err != nil {
			t.Fatal(err)
		}
		if record.GatewayID.Version() != 7 || gateways[record.GatewayID] ||
			record.BackendPath != "restfleet/agents/"+record.GatewayID.String()+"/"+record.ID.String() {
			t.Fatal("repository scope not independent")
		}
		gateways[record.GatewayID] = true
		for _, ref := range []uuid.UUID{record.GatewaySecretRef, record.ResticSecretRef} {
			var e domain.SecretEnvelope
			err := pool.QueryRow(ctx, `select id,kind,algorithm,key_id,ciphertext,nonce,wrapped_data_key,wrap_nonce,aad,created_at from secrets where id=$1`, ref).
				Scan(&e.ID, &e.Kind, &e.Algorithm, &e.KeyID, &e.Ciphertext, &e.Nonce, &e.WrappedDataKey, &e.WrapNonce, &e.AAD, &e.CreatedAt)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := security.OpenEnvelope(bytes.Repeat([]byte{8}, 32), security.Envelope{Ciphertext: e.Ciphertext, Nonce: e.Nonce, WrappedDataKey: e.WrappedDataKey, WrapNonce: e.WrapNonce, AAD: e.AAD})
			if err != nil || len(raw) != 43 || seen[string(raw)] {
				t.Fatal("repository passwords not independent encrypted 256-bit tokens")
			}
			if !bytes.Contains(e.AAD, []byte(record.HostID.String())) || !bytes.Contains(e.AAD, []byte(record.ID.String())) {
				t.Fatal("envelope not scoped")
			}
			seen[string(raw)] = true
			clear(raw)
		}
		res := b.request(t, http.MethodGet, "/api/v1/repositories/"+repo.Id.String(), nil, nil)
		if res.Code != http.StatusOK || res.Header().Get("ETag") != "\"1\"" {
			t.Fatal("detail failed")
		}
		assertRepositoryMetadata(t, res.Body.String())
	}
	var count int
	if err := pool.QueryRow(ctx, "select count(*) from repository_credential_revisions").Scan(&count); err != nil || count != 4 {
		t.Fatal("credential revisions missing")
	}
	var dump string
	if err := pool.QueryRow(ctx, `select jsonb_build_object('secrets',(select jsonb_agg(to_jsonb(s)) from secrets s),
		'audit',(select jsonb_agg(to_jsonb(a)) from audit_events a),'repositories',(select jsonb_agg(to_jsonb(r)) from repositories r))::text`).Scan(&dump); err != nil {
		t.Fatal(err)
	}
	for password := range seen {
		if strings.Contains(dump, password) || strings.Contains(logs.String(), password) {
			t.Fatal("plaintext leaked")
		}
	}
	assertNoStorageSecret(t, logs.String())
	var accesses int
	if err := pool.QueryRow(ctx, "select count(*) from audit_events where action='STORAGE_SECRET_ACCESS'").Scan(&accesses); err != nil || accesses != 0 {
		t.Fatal("creation must not decrypt cloud configuration")
	}
	if err := store.VerifyAuditChain(ctx); err != nil {
		t.Fatal(err)
	}
	var summary DashboardSummary
	decodeResponse(t, b.request(t, http.MethodGet, "/api/v1/dashboard/summary", nil, nil), &summary)
	if summary.Repositories != 2 {
		t.Fatal("dashboard repository count not updated")
	}
	var page RepositoryList
	decodeResponse(t, b.request(t, http.MethodGet, "/api/v1/repositories?limit=1", nil, nil), &page)
	if len(page.Items) != 1 || page.Items[0].Id != first.Id || page.NextCursor == nil {
		t.Fatal("first page incorrect")
	}
	var next RepositoryList
	decodeResponse(t, b.request(t, http.MethodGet, "/api/v1/repositories?limit=1&cursor="+*page.NextCursor, nil, nil), &next)
	if len(next.Items) != 1 || next.Items[0].Id != second.Id || next.NextCursor != nil {
		t.Fatal("pagination duplicated or skipped a repository")
	}
}

func TestRepositoryAuthorizationAndValidation(t *testing.T) {
	_, pool, handler, b, _ := setupFleet(t)
	host := createTestHost(t, b, "Validation Host")
	credential := createCredential(t, b, "Credential")
	headers := map[string]string{"X-CSRF-Token": b.cookies[csrfCookieName].Value}
	body := RepositoryCreate{Name: "Repo", HostId: host.Id, StorageCredentialId: credential.Id}
	if newTestBrowser(handler).request(t, http.MethodGet, "/api/v1/repositories", nil, nil).Code != http.StatusUnauthorized {
		t.Fatal("anonymous list allowed")
	}
	if b.request(t, http.MethodPost, "/api/v1/repositories", body, nil).Code != http.StatusForbidden {
		t.Fatal("missing CSRF allowed")
	}
	shared := true
	body.Shared = &shared
	res := b.request(t, http.MethodPost, "/api/v1/repositories", body, headers)
	if res.Code != http.StatusConflict || !strings.Contains(res.Body.String(), "SHARED_REPOSITORY_NOT_SUPPORTED") {
		t.Fatal("shared repository accepted")
	}
	body.Shared = nil
	for _, field := range []string{"status", "backend_path", "gateway_password", "restic_password", "host_ids"} {
		forbidden := map[string]any{"name": "Repo", "host_id": host.Id, "storage_credential_id": credential.Id, field: "client-controlled"}
		if b.request(t, http.MethodPost, "/api/v1/repositories", forbidden, headers).Code != http.StatusBadRequest {
			t.Fatal("client-controlled private field accepted")
		}
	}
	for _, name := range []string{"", "\t", "bad\nname"} {
		bad := body
		bad.Name = name
		if b.request(t, http.MethodPost, "/api/v1/repositories", bad, headers).Code != http.StatusUnprocessableEntity {
			t.Fatal("invalid name accepted")
		}
	}
	missing, _ := uuid.NewV7()
	bad := body
	bad.HostId = missing
	if b.request(t, http.MethodPost, "/api/v1/repositories", bad, headers).Code != http.StatusNotFound {
		t.Fatal("missing Host accepted")
	}
	bad = body
	bad.StorageCredentialId = missing
	if b.request(t, http.MethodPost, "/api/v1/repositories", bad, headers).Code != http.StatusNotFound {
		t.Fatal("missing credential accepted")
	}
	bad = body
	bad.HostId = uuid.Nil
	if b.request(t, http.MethodPost, "/api/v1/repositories", bad, headers).Code != http.StatusUnprocessableEntity {
		t.Fatal("missing host_id accepted")
	}
	if _, err := pool.Exec(context.Background(), "update hosts set status='DISABLED' where id=$1", host.Id); err != nil {
		t.Fatal(err)
	}
	if b.request(t, http.MethodPost, "/api/v1/repositories", body, headers).Code != http.StatusConflict {
		t.Fatal("disabled Host accepted")
	}
	if _, err := pool.Exec(context.Background(), "update hosts set status='PENDING' where id=$1", host.Id); err != nil {
		t.Fatal(err)
	}
	repo := createTestRepository(t, b, host.Id, credential.Id, "Owned")
	other := createTestHost(t, b, "Other Host")
	body.HostId = other.Id
	if _, err := pool.Exec(context.Background(), "update storage_credentials set status='DISABLED' where id=$1", credential.Id); err != nil {
		t.Fatal(err)
	}
	if b.request(t, http.MethodPost, "/api/v1/repositories", body, headers).Code != http.StatusConflict {
		t.Fatal("disabled credential accepted")
	}
	if _, err := pool.Exec(context.Background(), "update users set role='VIEWER'"); err != nil {
		t.Fatal(err)
	}
	if b.request(t, http.MethodPost, "/api/v1/repositories", body, headers).Code != http.StatusForbidden {
		t.Fatal("Viewer created repository")
	}
	for _, path := range []string{"/api/v1/repositories", "/api/v1/repositories/" + repo.Id.String()} {
		res := b.request(t, http.MethodGet, path, nil, nil)
		if res.Code != http.StatusOK {
			t.Fatal("Viewer cannot read metadata")
		}
		assertRepositoryMetadata(t, res.Body.String())
	}
	for _, query := range []string{"limit=0", "limit=201", "limit=1&limit=2", "cursor=bad", "cursor=bad&cursor=bad", "secret=true", "limit=1;cursor=bad"} {
		if b.request(t, http.MethodGet, "/api/v1/repositories?"+query, nil, nil).Code != http.StatusBadRequest {
			t.Fatal("invalid list query accepted")
		}
	}
	if b.request(t, http.MethodGet, "/api/v1/repositories/"+missing.String(), nil, nil).Code != http.StatusNotFound {
		t.Fatal("missing detail not 404")
	}
}

func TestRepositoryConcurrentCreateRollsBackLosingSecrets(t *testing.T) {
	_, pool, handler, b, _ := setupFleet(t)
	host := createTestHost(t, b, "Race Host")
	credential := createCredential(t, b, "Race credential")
	var before int
	if err := pool.QueryRow(context.Background(), "select count(*) from secrets").Scan(&before); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	codes := make(chan int, 2)
	for i := 0; i < 2; i++ {
		client := newTestBrowser(handler)
		for key, cookie := range b.cookies {
			client.cookies[key] = cookie
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			res := client.request(t, http.MethodPost, "/api/v1/repositories", RepositoryCreate{Name: "Race", HostId: host.Id, StorageCredentialId: credential.Id},
				map[string]string{"X-CSRF-Token": client.cookies[csrfCookieName].Value})
			codes <- res.Code
		}()
	}
	wg.Wait()
	close(codes)
	counts := map[int]int{}
	for code := range codes {
		counts[code]++
	}
	if counts[http.StatusCreated] != 1 || counts[http.StatusConflict] != 1 {
		t.Fatalf("concurrent results=%v", counts)
	}
	var repos, versions, secrets, audits int
	if err := pool.QueryRow(context.Background(), `select (select count(*) from repositories),(select count(*) from repository_credential_revisions),
		(select count(*) from secrets),(select count(*) from audit_events where action='REPOSITORY_CREATE')`).Scan(&repos, &versions, &secrets, &audits); err != nil {
		t.Fatal(err)
	}
	if repos != 1 || versions != 2 || secrets != before+2 || audits != 1 {
		t.Fatal("losing request committed partial records")
	}
}

func TestRepositoryAuditFailureRollsBackEverything(t *testing.T) {
	store, pool, _, b, _ := setupFleet(t)
	host := createTestHost(t, b, "Rollback Host")
	credential := createCredential(t, b, "Rollback credential")
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `create function reject_repository_audit() returns trigger language plpgsql as $$
		begin if NEW.action='REPOSITORY_CREATE' then raise exception 'audit unavailable'; end if; return NEW; end $$;
		create trigger repository_audit_failure before insert on audit_events for each row execute function reject_repository_audit();`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(ctx, "drop trigger repository_audit_failure on audit_events; drop function reject_repository_audit()"); err != nil {
			t.Error(err)
		}
	})
	var before int
	if err := pool.QueryRow(ctx, "select count(*) from secrets").Scan(&before); err != nil {
		t.Fatal(err)
	}
	res := b.request(t, http.MethodPost, "/api/v1/repositories", RepositoryCreate{Name: "Rollback", HostId: host.Id, StorageCredentialId: credential.Id},
		map[string]string{"X-CSRF-Token": b.cookies[csrfCookieName].Value})
	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("audit failure=%d", res.Code)
	}
	var repos, versions, secrets int
	if err := pool.QueryRow(ctx, `select (select count(*) from repositories),(select count(*) from repository_credential_revisions),(select count(*) from secrets)`).Scan(&repos, &versions, &secrets); err != nil {
		t.Fatal(err)
	}
	if repos != 0 || versions != 0 || secrets != before {
		t.Fatal("audit failure committed partial repository")
	}
	if err := store.VerifyAuditChain(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestRepositoryResponseDoesNotSerializeReferences(t *testing.T) {
	raw, err := json.Marshal(repositoryResponse(domain.Repository{GatewaySecretRef: uuid.New(), ResticSecretRef: uuid.New(), GatewayID: uuid.New(), BackendPath: "private"}))
	if err != nil {
		t.Fatal(err)
	}
	assertRepositoryMetadata(t, string(raw))
}

func TestRepositoryUnavailableStateDoesNotReleaseHost(t *testing.T) {
	_, pool, _, b, _ := setupFleet(t)
	host := createTestHost(t, b, "Owned Host")
	credential := createCredential(t, b, "Owned credential")
	repo := createTestRepository(t, b, host.Id, credential.Id, "Owned")
	for _, status := range []string{"DISABLED", "ERROR"} {
		if _, err := pool.Exec(context.Background(), "update repositories set status=$1 where id=$2", status, repo.Id); err != nil {
			t.Fatal(err)
		}
		res := b.request(t, http.MethodPost, "/api/v1/repositories",
			RepositoryCreate{Name: "Duplicate", HostId: host.Id, StorageCredentialId: credential.Id},
			map[string]string{"X-CSRF-Token": b.cookies[csrfCookieName].Value})
		if res.Code != http.StatusConflict || !strings.Contains(res.Body.String(), "HOST_REPOSITORY_EXISTS") {
			t.Fatalf("%s released repository ownership: %d", status, res.Code)
		}
	}
}

func TestRepositoryOpenAPIContract(t *testing.T) {
	spec, err := GetSwagger()
	if err != nil {
		t.Fatal(err)
	}
	if err := spec.Validate(context.Background()); err != nil {
		t.Fatal(err)
	}
	schema := spec.Components.Schemas["Repository"].Value
	value := map[string]any{
		"id": uuid.NewString(), "name": "Archive", "host_id": uuid.NewString(), "storage_credential_id": uuid.NewString(),
		"status": "PROVISIONING", "gateway_secret_revision": float64(1), "restic_secret_revision": float64(1),
		"revision": float64(1), "created_at": "2026-09-07T00:00:00Z", "updated_at": "2026-09-07T00:00:00Z",
	}
	if err := schema.VisitJSON(value); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"backend_path", "gateway_username", "gateway_password", "restic_password", "secret_ref", "ciphertext"} {
		value[field] = "forbidden"
		if err := schema.VisitJSON(value); err == nil {
			t.Fatalf("contract permits %s", field)
		}
		delete(value, field)
	}
	value["format_version"] = float64(3)
	if err := schema.VisitJSON(value); err == nil {
		t.Fatal("unknown repository format accepted by contract")
	}
	value["format_version"] = float64(2)
	if err := schema.VisitJSON(value); err != nil {
		t.Fatal(err)
	}
	delete(value, "host_id")
	if err := schema.VisitJSON(value); err == nil {
		t.Fatal("Host ownership optional in contract")
	}
}
