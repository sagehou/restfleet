package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sagehou/restfleet/internal/persistence/postgres"
	"github.com/sagehou/restfleet/internal/security"
	control "github.com/sagehou/restfleet/internal/server"
)

type testBrowser struct {
	handler http.Handler
	cookies map[string]*http.Cookie
	remote  string
}

func newTestBrowser(handler http.Handler) *testBrowser {
	return &testBrowser{
		handler: handler,
		cookies: make(map[string]*http.Cookie),
		remote:  "192.0.2.10:4567",
	}
}

func (b *testBrowser) request(
	t *testing.T,
	method string,
	path string,
	body any,
	headers map[string]string,
) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(encoded)
	}
	request := httptest.NewRequest(method, path, reader)
	request.RemoteAddr = b.remote
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	for _, cookie := range b.cookies {
		request.AddCookie(cookie)
	}
	recorder := httptest.NewRecorder()
	b.handler.ServeHTTP(recorder, request)
	for _, cookie := range recorder.Result().Cookies() {
		if cookie.MaxAge < 0 {
			delete(b.cookies, cookie.Name)
		} else {
			b.cookies[cookie.Name] = cookie
		}
	}
	return recorder
}

func setupIntegration(
	t *testing.T,
	bootstrapToken string,
	clock *time.Time,
) (*postgres.Store, *pgxpool.Pool, http.Handler, *bytes.Buffer) {
	t.Helper()
	databaseURL := os.Getenv("RESTFLEET_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("RESTFLEET_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	adminPool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(adminPool.Close)
	_, err = adminPool.Exec(ctx, `
		truncate table outbox_events, agent_inventories, agent_desired_states,
			server_pki, secrets, agent_certificates, enrollment_tokens,
			agents, hosts, audit_events, sessions, bootstrap_state, users restart identity cascade;
		insert into bootstrap_state (singleton, created_at) values (true, now());
	`)
	if err != nil {
		t.Fatalf("reset integration database: %v", err)
	}
	store, err := postgres.Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	params := security.Argon2Params{
		Memory:      64,
		Iterations:  1,
		Parallelism: 1,
		SaltLength:  8,
		KeyLength:   16,
	}
	agentCA, agentCAPrivatePEM, err := security.NewAgentCA(*clock)
	if err != nil {
		t.Fatal(err)
	}
	clear(agentCAPrivatePEM)
	controlPlane, err := control.NewControlPlane(store, control.Settings{
		BootstrapToken: bootstrapToken,
		IdleTTL:        5 * time.Minute,
		AbsoluteTTL:    time.Hour,
		PasswordParams: params,
		ExpectedSchema: postgres.ExpectedSchemaVersion,
		Clock:          func() time.Time { return *clock },
		Enrollment: control.EnrollmentSettings{
			Pepper: bytes.Repeat([]byte{7}, 32),
			CA:     agentCA, PublicURL: "https://control.example",
			GRPCEndpoint: "control.example:443", ServerName: "control.example",
			ServerCABundlePEM: agentCA.CertificatePEM(),
			HeartbeatInterval: 15 * time.Second,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	logOutput := &bytes.Buffer{}
	logger := slog.New(slog.NewJSONHandler(logOutput, nil))
	api := New(controlPlane, Options{
		SecureCookies: false,
		Logger:        logger,
		Build:         BuildInfo{Version: "test", Commit: "test-commit", Date: "2026-09-03T00:00:00Z"},
	})
	return store, adminPool, api.NewRootHandler(), logOutput
}

func bootstrapAdmin(t *testing.T, browser *testBrowser, token string, password string) *httptest.ResponseRecorder {
	t.Helper()
	return browser.request(t, http.MethodPost, "/api/v1/bootstrap", BootstrapRequest{
		Username:    "admin",
		DisplayName: "Administrator",
		Password:    password,
	}, map[string]string{"X-RestFleet-Bootstrap-Token": token})
}

func TestAuthLifecycleAndCSRF(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Microsecond)
	store, pool, handler, _ := setupIntegration(t, "bootstrap-test-token", &now)
	browser := newTestBrowser(handler)

	missingToken := browser.request(t, http.MethodPost, "/api/v1/bootstrap", BootstrapRequest{
		Username: "admin", DisplayName: "Administrator", Password: "a-strong-test-password",
	}, nil)
	if missingToken.Code != http.StatusForbidden {
		t.Fatalf("missing bootstrap token status = %d, body = %s", missingToken.Code, missingToken.Body.String())
	}
	var bootstrapAuditCount int
	if err := pool.QueryRow(context.Background(), `
		select count(*) from audit_events
		where action = 'AUTH_BOOTSTRAP' and result = 'DENIED' and reason_code = 'TOKEN_MISSING'
	`).Scan(&bootstrapAuditCount); err != nil {
		t.Fatal(err)
	}
	if bootstrapAuditCount != 1 {
		t.Fatalf("missing bootstrap token audit count = %d, want 1", bootstrapAuditCount)
	}

	response := bootstrapAdmin(t, browser, "bootstrap-test-token", "a-strong-test-password")
	if response.Code != http.StatusCreated {
		t.Fatalf("bootstrap status = %d, body = %s", response.Code, response.Body.String())
	}
	sessionCookie := browser.cookies[sessionCookieName]
	csrfCookie := browser.cookies[csrfCookieName]
	if sessionCookie == nil || !sessionCookie.HttpOnly || sessionCookie.SameSite != http.SameSiteLaxMode {
		t.Fatal("session cookie security attributes are missing")
	}
	if csrfCookie == nil || csrfCookie.HttpOnly || csrfCookie.Value == "" {
		t.Fatal("CSRF cookie is not browser-readable or is empty")
	}
	if strings.Contains(response.Body.String(), "a-strong-test-password") ||
		strings.Contains(response.Body.String(), sessionCookie.Value) {
		t.Fatal("bootstrap response leaked credentials")
	}

	response = bootstrapAdmin(t, browser, "bootstrap-test-token", "another-strong-password")
	if response.Code != http.StatusConflict {
		t.Fatalf("second bootstrap status = %d, body = %s", response.Code, response.Body.String())
	}
	var closedAuditCount int
	if err := pool.QueryRow(context.Background(), "select count(*) from audit_events where reason_code = 'BOOTSTRAP_CLOSED'").Scan(&closedAuditCount); err != nil {
		t.Fatal(err)
	}
	if closedAuditCount != 1 {
		t.Fatalf("closed bootstrap audit count = %d, want 1", closedAuditCount)
	}

	response = browser.request(t, http.MethodPost, "/api/v1/auth/logout", nil, nil)
	if response.Code != http.StatusForbidden {
		t.Fatalf("missing CSRF status = %d, body = %s", response.Code, response.Body.String())
	}
	response = browser.request(t, http.MethodGet, "/api/v1/auth/session", nil, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("CSRF denial changed session: status = %d", response.Code)
	}

	response = browser.request(t, http.MethodPost, "/api/v1/auth/logout", nil, map[string]string{"X-CSRF-Token": "wrong"})
	if response.Code != http.StatusForbidden {
		t.Fatalf("wrong CSRF status = %d, body = %s", response.Code, response.Body.String())
	}
	response = browser.request(t, http.MethodPost, "/api/v1/auth/logout", nil, map[string]string{"X-CSRF-Token": csrfCookie.Value})
	if response.Code != http.StatusNoContent {
		t.Fatalf("logout status = %d, body = %s", response.Code, response.Body.String())
	}
	response = browser.request(t, http.MethodGet, "/api/v1/auth/session?token=must-not-appear", nil, nil)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("revoked session status = %d", response.Code)
	}
	var authorizationProblem Problem
	if err := json.Unmarshal(response.Body.Bytes(), &authorizationProblem); err != nil {
		t.Fatal(err)
	}
	if authorizationProblem.Instance != "/api/v1/auth/session" || strings.Contains(response.Body.String(), "must-not-appear") {
		t.Fatal("problem response leaked the query string")
	}
	response = browser.request(t, http.MethodPost, "/api/v1/auth/login", LoginRequest{
		Username: "admin",
		Password: "a-strong-test-password",
	}, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("login status = %d, body = %s", response.Code, response.Body.String())
	}
	response = browser.request(t, http.MethodGet, "/api/v1/dashboard/summary", nil, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("dashboard status = %d, body = %s", response.Code, response.Body.String())
	}
	if err := store.VerifyAuditChain(context.Background()); err != nil {
		t.Fatalf("audit chain invalid: %v", err)
	}
}

func TestLoginFailureIsUniformAndRateLimited(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Microsecond)
	_, _, handler, _ := setupIntegration(t, "bootstrap-test-token", &now)
	browser := newTestBrowser(handler)
	if response := bootstrapAdmin(t, browser, "bootstrap-test-token", "a-strong-test-password"); response.Code != http.StatusCreated {
		t.Fatalf("bootstrap status = %d", response.Code)
	}
	browser.cookies = make(map[string]*http.Cookie)

	known := browser.request(t, http.MethodPost, "/api/v1/auth/login", LoginRequest{
		Username: "admin",
		Password: "wrong-password",
	}, nil)
	unknown := browser.request(t, http.MethodPost, "/api/v1/auth/login", LoginRequest{
		Username: "not-a-user",
		Password: "wrong-password",
	}, nil)
	var knownProblem, unknownProblem Problem
	if err := json.Unmarshal(known.Body.Bytes(), &knownProblem); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(unknown.Body.Bytes(), &unknownProblem); err != nil {
		t.Fatal(err)
	}
	if known.Code != http.StatusUnauthorized || unknown.Code != http.StatusUnauthorized ||
		knownProblem.Code != unknownProblem.Code || knownProblem.Detail != unknownProblem.Detail {
		t.Fatalf("login failure disclosed account state: known=%+v unknown=%+v", knownProblem, unknownProblem)
	}

	for range 3 {
		_ = browser.request(t, http.MethodPost, "/api/v1/auth/login", LoginRequest{
			Username: "admin",
			Password: "wrong-password",
		}, nil)
	}
	limited := browser.request(t, http.MethodPost, "/api/v1/auth/login", LoginRequest{
		Username: "admin",
		Password: "wrong-password",
	}, nil)
	if limited.Code != http.StatusTooManyRequests {
		t.Fatalf("rate-limit status = %d, body = %s", limited.Code, limited.Body.String())
	}
}

func TestExpiredSessionIsRevoked(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Microsecond)
	store, pool, handler, _ := setupIntegration(t, "bootstrap-test-token", &now)
	browser := newTestBrowser(handler)
	if response := bootstrapAdmin(t, browser, "bootstrap-test-token", "a-strong-test-password"); response.Code != http.StatusCreated {
		t.Fatalf("bootstrap status = %d", response.Code)
	}
	now = now.Add(6 * time.Minute)
	response := browser.request(t, http.MethodGet, "/api/v1/auth/session", nil, nil)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("expired session status = %d, body = %s", response.Code, response.Body.String())
	}
	var revoked int
	if err := pool.QueryRow(context.Background(), "select count(*) from sessions where revoked_at is not null").Scan(&revoked); err != nil {
		t.Fatal(err)
	}
	if revoked != 1 {
		t.Fatalf("revoked sessions = %d, want 1", revoked)
	}
	if err := store.VerifyAuditChain(context.Background()); err != nil {
		t.Fatalf("audit chain invalid after expiry: %v", err)
	}
}

func TestAbsoluteSessionExpiryCapsSlidingIdleTTL(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Microsecond)
	_, pool, handler, _ := setupIntegration(t, "bootstrap-test-token", &now)
	browser := newTestBrowser(handler)
	if response := bootstrapAdmin(t, browser, "bootstrap-test-token", "a-strong-test-password"); response.Code != http.StatusCreated {
		t.Fatalf("bootstrap status = %d", response.Code)
	}

	for range 14 {
		now = now.Add(4 * time.Minute)
		if response := browser.request(t, http.MethodGet, "/api/v1/auth/session", nil, nil); response.Code != http.StatusOK {
			t.Fatalf("sliding session rejected before absolute expiry: status = %d", response.Code)
		}
	}
	now = now.Add(5 * time.Minute)
	if response := browser.request(t, http.MethodGet, "/api/v1/auth/session", nil, nil); response.Code != http.StatusUnauthorized {
		t.Fatalf("session past absolute expiry status = %d", response.Code)
	}
	var revoked int
	if err := pool.QueryRow(context.Background(), "select count(*) from sessions where revoked_at is not null").Scan(&revoked); err != nil {
		t.Fatal(err)
	}
	if revoked != 1 {
		t.Fatalf("revoked sessions = %d, want 1", revoked)
	}
}

func TestCanarySecretDoesNotReachResponseLogOrAudit(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Microsecond)
	canary := "restfleet-canary-secret-9f6a"
	store, _, handler, logs := setupIntegration(t, canary, &now)
	browser := newTestBrowser(handler)
	response := bootstrapAdmin(t, browser, canary, canary+"-password")
	if response.Code != http.StatusCreated {
		t.Fatalf("bootstrap status = %d, body = %s", response.Code, response.Body.String())
	}
	browser.cookies = make(map[string]*http.Cookie)
	failure := browser.request(t, http.MethodPost, "/api/v1/auth/login", LoginRequest{
		Username: "admin",
		Password: canary,
	}, nil)
	events, err := store.AuditEvents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	auditJSON, err := json.Marshal(events)
	if err != nil {
		t.Fatal(err)
	}
	combined := response.Body.String() + failure.Body.String() + logs.String() + string(auditJSON)
	if strings.Contains(combined, canary) {
		t.Fatal("canary secret leaked through response, log, or audit")
	}
}

func TestAuditChainDetectsDatabaseMutation(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Microsecond)
	store, pool, handler, _ := setupIntegration(t, "bootstrap-test-token", &now)
	browser := newTestBrowser(handler)
	if response := bootstrapAdmin(t, browser, "bootstrap-test-token", "a-strong-test-password"); response.Code != http.StatusCreated {
		t.Fatalf("bootstrap status = %d", response.Code)
	}
	if err := store.VerifyAuditChain(context.Background()); err != nil {
		t.Fatalf("valid chain rejected: %v", err)
	}
	if _, err := pool.Exec(context.Background(), "update audit_events set reason_code = 'TAMPERED' where sequence = 1"); err != nil {
		t.Fatal(err)
	}
	if err := store.VerifyAuditChain(context.Background()); err == nil {
		t.Fatal("mutated audit row was not detected")
	}
}

type failingPingStore struct {
	control.Store
}

func (failingPingStore) Ping(context.Context) error {
	return errors.New("database unavailable")
}

func TestReadinessFailsWhenDatabaseIsUnavailable(t *testing.T) {
	controlPlane, err := control.NewControlPlane(failingPingStore{}, control.Settings{
		BootstrapToken: "unused",
		PasswordParams: security.Argon2Params{
			Memory: 64, Iterations: 1, Parallelism: 1, SaltLength: 8, KeyLength: 16,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	api := New(controlPlane, Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	request := httptest.NewRequest(http.MethodGet, "/health/ready", nil)
	recorder := httptest.NewRecorder()
	api.NewRootHandler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("readiness status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var problem Problem
	if err := json.Unmarshal(recorder.Body.Bytes(), &problem); err != nil {
		t.Fatal(err)
	}
	if problem.Code != "NOT_READY" {
		t.Fatalf("readiness problem code = %q", problem.Code)
	}
}
