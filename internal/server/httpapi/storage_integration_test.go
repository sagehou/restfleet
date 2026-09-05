package httpapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sagehou/restfleet/internal/domain"
	"github.com/sagehou/restfleet/internal/security"
)

func credentialFixture() string {
	return "[cloud]\ntype = onedrive\ntoken = {\"access_token\":\"storage-access-canary\",\"token_type\":\"Bearer\",\"refresh_token\":\"storage-refresh-canary\",\"expiry\":\"2030-01-01T00:00:00Z\"}\ndrive_id = example-drive\ndrive_type = personal\n[encrypted]\ntype = crypt\nremote = cloud:backups\npassword = " + base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{5}, 32)) + "\n"
}

func createCredential(t *testing.T, b *testBrowser, name string) StorageCredential {
	t.Helper()
	r := b.request(t, http.MethodPost, "/api/v1/storage-credentials", StorageCredentialCreate{Name: name, RemoteName: "encrypted", RcloneConfig: credentialFixture()}, map[string]string{"X-CSRF-Token": b.cookies[csrfCookieName].Value})
	if r.Code != http.StatusCreated {
		t.Fatalf("credential create = %d, want 201", r.Code)
	}
	if r.Header().Get("ETag") != "\"1\"" || r.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("credential headers missing")
	}
	assertNoStorageSecret(t, r.Body.String())
	var c StorageCredential
	decodeResponse(t, r, &c)
	return c
}

func assertNoStorageSecret(t *testing.T, value string) {
	t.Helper()
	for _, marker := range []string{"storage-access-canary", "storage-refresh-canary", "storage-new-canary", `"rclone_config":`, "ciphertext", "wrapped_data_key", "secret_ref", base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{5}, 32))} {
		if strings.Contains(value, marker) {
			t.Fatal("secret material reached metadata or diagnostics")
		}
	}
}

func TestStorageCredentialLifecycleAndSecretBoundary(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Microsecond)
	store, pool, handler, logs := setupIntegration(t, "bootstrap-test-token", &now)
	b := newTestBrowser(handler)
	if bootstrapAdmin(t, b, "bootstrap-test-token", "a-strong-test-password").Code != http.StatusCreated {
		t.Fatal("bootstrap failed")
	}
	c := createCredential(t, b, "Primary archive")
	if c.Id.Version() != 7 || c.Status != StorageCredentialStatusUNTESTED || c.SecretRevision != 1 {
		t.Fatal("invalid initial credential state")
	}
	ctx := context.Background()
	current, err := store.StorageCredential(ctx, c.Id)
	if err != nil {
		t.Fatal(err)
	}
	secret, err := store.StorageCredentialSecret(ctx, current.SecretRef)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(secret.Ciphertext, []byte("storage-access-canary")) {
		t.Fatal("plaintext persisted")
	}
	plaintext, err := security.OpenEnvelope(bytes.Repeat([]byte{8}, 32), security.Envelope{
		Ciphertext: secret.Ciphertext, Nonce: secret.Nonce, WrappedDataKey: secret.WrappedDataKey, WrapNonce: secret.WrapNonce, AAD: secret.AAD,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer clear(plaintext)
	if !bytes.Contains(plaintext, []byte("storage-access-canary")) {
		t.Fatal("wrong secret encrypted")
	}
	if _, err := security.OpenEnvelope(bytes.Repeat([]byte{9}, 32), security.Envelope{
		Ciphertext: secret.Ciphertext, Nonce: secret.Nonce, WrappedDataKey: secret.WrappedDataKey, WrapNonce: secret.WrapNonce, AAD: secret.AAD,
	}); err == nil {
		t.Fatal("wrong master key decrypted the credential")
	}

	endpoint := "/api/v1/storage-credentials/" + c.Id.String()
	headers := map[string]string{"X-CSRF-Token": b.cookies[csrfCookieName].Value, "If-Match": "\"1\""}
	r := b.request(t, http.MethodPost, endpoint+"/replace-secret", StorageCredentialReplace{RcloneConfig: strings.Replace(credentialFixture(), "cloud:backups", "cloud:other", 1)}, headers)
	if r.Code != http.StatusConflict {
		t.Fatalf("target change = %d", r.Code)
	}
	assertNoStorageSecret(t, r.Body.String())
	next := strings.Replace(credentialFixture(), "storage-refresh-canary", "storage-new-canary", 1)
	r = b.request(t, http.MethodPost, endpoint+"/replace-secret", StorageCredentialReplace{RcloneConfig: next}, headers)
	if r.Code != http.StatusOK {
		t.Fatalf("replace = %d", r.Code)
	}
	assertNoStorageSecret(t, r.Body.String())
	decodeResponse(t, r, &c)
	if c.Revision != 2 || c.SecretRevision != 2 || c.Status != StorageCredentialStatusUNTESTED {
		t.Fatal("replacement revisions incorrect")
	}
	if b.request(t, http.MethodPost, endpoint+"/replace-secret", StorageCredentialReplace{RcloneConfig: next}, headers).Code != http.StatusPreconditionFailed {
		t.Fatal("stale replacement was accepted")
	}
	if b.request(t, http.MethodPost, endpoint+"/disable", nil, headers).Code != http.StatusPreconditionFailed {
		t.Fatal("stale disable was accepted")
	}
	headers["If-Match"] = "\"2\""
	r = b.request(t, http.MethodPost, endpoint+"/disable", nil, headers)
	if r.Code != http.StatusOK {
		t.Fatalf("disable = %d", r.Code)
	}
	decodeResponse(t, r, &c)
	if c.Revision != 3 || c.SecretRevision != 2 || c.Status != StorageCredentialStatusDISABLED {
		t.Fatal("disable changed secret version")
	}
	headers["If-Match"] = "\"3\""
	if b.request(t, http.MethodPost, endpoint+"/replace-secret", StorageCredentialReplace{RcloneConfig: next}, headers).Code != http.StatusConflict {
		t.Fatal("disabled credential was reactivated")
	}
	for _, path := range []string{endpoint, "/api/v1/storage-credentials"} {
		r = b.request(t, http.MethodGet, path, nil, nil)
		if r.Code != http.StatusOK {
			t.Fatal("metadata unavailable")
		}
		assertNoStorageSecret(t, r.Body.String())
	}
	var revisions int
	if err := pool.QueryRow(ctx, "select count(*) from storage_credential_revisions where credential_id=$1", c.Id).Scan(&revisions); err != nil {
		t.Fatal(err)
	}
	if revisions != 2 {
		t.Fatal("secret history was lost or duplicated")
	}
	var auditJSON string
	if err := pool.QueryRow(ctx, "select coalesce(jsonb_agg(to_jsonb(a))::text,'[]') from audit_events a").Scan(&auditJSON); err != nil {
		t.Fatal(err)
	}
	assertNoStorageSecret(t, auditJSON)
	assertNoStorageSecret(t, logs.String())
	if err := store.VerifyAuditChain(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestStorageCredentialAuthorizationAndValidation(t *testing.T) {
	_, pool, handler, b, _ := setupFleet(t)
	c := createCredential(t, b, "Access test")
	create := StorageCredentialCreate{Name: "Other", RemoteName: "encrypted", RcloneConfig: credentialFixture()}
	headers := map[string]string{"X-CSRF-Token": b.cookies[csrfCookieName].Value, "If-Match": "\"1\""}
	if newTestBrowser(handler).request(t, http.MethodGet, "/api/v1/storage-credentials", nil, nil).Code != http.StatusUnauthorized {
		t.Fatal("anonymous metadata access")
	}
	for _, csrf := range []string{"", "wrong"} {
		r := b.request(t, http.MethodPost, "/api/v1/storage-credentials", create, map[string]string{"X-CSRF-Token": csrf})
		if r.Code != http.StatusForbidden {
			t.Fatal("CSRF boundary bypassed")
		}
	}
	invalid := create
	invalid.RcloneConfig += "token_url = http://169.254.169.254/storage-access-canary\n"
	r := b.request(t, http.MethodPost, "/api/v1/storage-credentials", invalid, headers)
	if r.Code != http.StatusUnprocessableEntity {
		t.Fatal("unsafe import accepted")
	}
	assertNoStorageSecret(t, r.Body.String())
	for _, query := range []string{"?limit=0", "?limit=201", "?cursor=bad", "?sort=name", "?limit=1&limit=2"} {
		if b.request(t, http.MethodGet, "/api/v1/storage-credentials"+query, nil, nil).Code != http.StatusBadRequest {
			t.Fatal("invalid list query accepted")
		}
	}
	if _, err := pool.Exec(context.Background(), "update users set role='VIEWER'"); err != nil {
		t.Fatal(err)
	}
	for _, operation := range []struct {
		path string
		body any
	}{
		{"/api/v1/storage-credentials", create},
		{"/api/v1/storage-credentials/" + c.Id.String() + "/replace-secret", StorageCredentialReplace{RcloneConfig: credentialFixture()}},
		{"/api/v1/storage-credentials/" + c.Id.String() + "/disable", nil},
		{"/api/v1/hosts", HostCreate{DisplayName: "Denied", Timezone: "UTC"}},
	} {
		if b.request(t, http.MethodPost, operation.path, operation.body, headers).Code != http.StatusForbidden {
			t.Fatal("VIEWER mutation allowed")
		}
	}
	if b.request(t, http.MethodGet, "/api/v1/storage-credentials", nil, nil).Code != http.StatusOK {
		t.Fatal("VIEWER metadata denied")
	}
	var count int
	if err := pool.QueryRow(context.Background(), "select count(*) from storage_credentials").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatal("denied operations changed storage")
	}
}

func TestStorageCredentialConcurrentReplacement(t *testing.T) {
	_, pool, handler, b, _ := setupFleet(t)
	c := createCredential(t, b, "Concurrency")
	var wg sync.WaitGroup
	results := make(chan int, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			request := httptest.NewRequest(http.MethodPost, "/api/v1/storage-credentials/"+c.Id.String()+"/replace-secret", encodeBody(t, StorageCredentialReplace{RcloneConfig: credentialFixture()}))
			request.Header.Set("X-CSRF-Token", b.cookies[csrfCookieName].Value)
			request.Header.Set("If-Match", "\"1\"")
			for _, cookie := range b.cookies {
				request.AddCookie(cookie)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			results <- response.Code
		}()
	}
	wg.Wait()
	close(results)
	counts := map[int]int{}
	for code := range results {
		counts[code]++
	}
	if counts[http.StatusOK] != 1 || counts[http.StatusPreconditionFailed] != 1 {
		t.Fatalf("concurrent status counts = %v", counts)
	}
	var versions int
	if err := pool.QueryRow(context.Background(), "select count(*) from storage_credential_revisions where credential_id=$1", c.Id).Scan(&versions); err != nil {
		t.Fatal(err)
	}
	if versions != 2 {
		t.Fatal("concurrent replacement persisted extra versions")
	}
}

func TestStorageCredentialAuditFailureRollsBack(t *testing.T) {
	_, pool, _, b, _ := setupFleet(t)
	ctx := context.Background()
	_, err := pool.Exec(ctx, `
		create function reject_storage_audit() returns trigger language plpgsql as $$
		begin
		  if NEW.action = 'STORAGE_CREDENTIAL_CREATE' then raise exception 'audit unavailable'; end if;
		  return NEW;
		end $$;
		create trigger storage_audit_failure before insert on audit_events
		for each row execute function reject_storage_audit();
	`)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(ctx, "drop trigger storage_audit_failure on audit_events; drop function reject_storage_audit()"); err != nil {
			t.Error(err)
		}
	})
	var before int
	if err := pool.QueryRow(ctx, "select count(*) from secrets").Scan(&before); err != nil {
		t.Fatal(err)
	}
	r := b.request(t, http.MethodPost, "/api/v1/storage-credentials", StorageCredentialCreate{Name: "Rollback", RemoteName: "encrypted", RcloneConfig: credentialFixture()}, map[string]string{"X-CSRF-Token": b.cookies[csrfCookieName].Value})
	if r.Code != http.StatusServiceUnavailable {
		t.Fatalf("audit failure = %d", r.Code)
	}
	var credentials, secrets int
	if err := pool.QueryRow(ctx, "select (select count(*) from storage_credentials),(select count(*) from secrets)").Scan(&credentials, &secrets); err != nil {
		t.Fatal(err)
	}
	if credentials != 0 || secrets != before {
		t.Fatal("audit failure committed a partial credential")
	}
}

func TestStorageCredentialPagination(t *testing.T) {
	_, _, _, b, _ := setupFleet(t)
	first := createCredential(t, b, "First")
	second := createCredential(t, b, "Second")
	r := b.request(t, http.MethodGet, "/api/v1/storage-credentials?limit=1", nil, nil)
	var page StorageCredentialList
	decodeResponse(t, r, &page)
	if len(page.Items) != 1 || page.Items[0].Id != first.Id || page.NextCursor == nil {
		t.Fatal("first page incorrect")
	}
	r = b.request(t, http.MethodGet, "/api/v1/storage-credentials?limit=1&cursor="+*page.NextCursor, nil, nil)
	page = StorageCredentialList{}
	decodeResponse(t, r, &page)
	if len(page.Items) != 1 || page.Items[0].Id != second.Id || page.NextCursor != nil {
		t.Fatal("pagination repeated or skipped an item")
	}
	if r = b.request(t, http.MethodGet, "/api/v1/storage-credentials/"+uuid.NewString(), nil, nil); r.Code != http.StatusNotFound {
		t.Fatal("missing credential did not return 404")
	}
}

func TestStorageCredentialResponseMetadataOnly(t *testing.T) {
	encoded, err := json.Marshal(storageCredentialResponse(domain.StorageCredential{ID: uuid.New(), SecretRef: uuid.New()}))
	if err != nil {
		t.Fatal(err)
	}
	assertNoStorageSecret(t, string(encoded))
}
