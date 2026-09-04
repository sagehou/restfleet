package httpapi

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sagehou/restfleet/internal/domain"
	"github.com/sagehou/restfleet/internal/persistence/postgres"
	"github.com/sagehou/restfleet/internal/security"
)

func decodeResponse(t *testing.T, recorder *httptest.ResponseRecorder, destination any) {
	t.Helper()
	if err := json.Unmarshal(recorder.Body.Bytes(), destination); err != nil {
		t.Fatal(err)
	}
}

func encodeBody(t *testing.T, value any) *bytes.Reader {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.NewReader(encoded)
}

func setupFleet(
	t *testing.T,
) (*postgres.Store, *pgxpool.Pool, http.Handler, *testBrowser, time.Time) {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Microsecond)
	store, pool, handler, _ := setupIntegration(t, "bootstrap-test-token", &now)
	browser := newTestBrowser(handler)
	response := bootstrapAdmin(t, browser, "bootstrap-test-token", "a-strong-test-password")
	if response.Code != http.StatusCreated {
		t.Fatalf("bootstrap status = %d, body = %s", response.Code, response.Body.String())
	}
	return store, pool, handler, browser, now
}

func createTestHost(t *testing.T, browser *testBrowser, name string) Host {
	t.Helper()
	response := browser.request(t, http.MethodPost, "/api/v1/hosts", HostCreate{
		DisplayName: name,
		Timezone:    "UTC",
	}, map[string]string{"X-CSRF-Token": browser.cookies[csrfCookieName].Value})
	if response.Code != http.StatusCreated {
		t.Fatalf("create Host status = %d, body = %s", response.Code, response.Body.String())
	}
	var host Host
	decodeResponse(t, response, &host)
	return host
}

func createTestEnrollmentToken(t *testing.T, browser *testBrowser, hostID uuid.UUID) EnrollmentTokenCreated {
	t.Helper()
	response := browser.request(
		t, http.MethodPost, "/api/v1/hosts/"+hostID.String()+"/enrollment-tokens",
		EnrollmentTokenCreate{},
		map[string]string{"X-CSRF-Token": browser.cookies[csrfCookieName].Value},
	)
	if response.Code != http.StatusCreated {
		t.Fatalf("create enrollment token status = %d, body = %s", response.Code, response.Body.String())
	}
	var token EnrollmentTokenCreated
	decodeResponse(t, response, &token)
	return token
}

func agentCSR(t *testing.T) (string, []byte) {
	t.Helper()
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	privateDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	privatePEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER})
	clear(privateDER)
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER})), privatePEM
}

func enrollRequest(token, csr string, installID uuid.UUID) AgentEnrollmentRequest {
	return AgentEnrollmentRequest{
		Token: token, CsrPem: csr, InstallId: installID,
		AgentVersion: "0.2.0-test", ProtocolVersion: "1.0",
		Hostname: "agent-test", Os: Linux, Arch: AgentEnrollmentRequestArchAmd64,
		Capabilities: []string{"certificate_rotation_v1"},
	}
}

func TestEnrollmentTokenAndCertificateLifecycle(t *testing.T) {
	store, pool, _, browser, now := setupFleet(t)
	host := createTestHost(t, browser, "edge-01")
	token := createTestEnrollmentToken(t, browser, host.Id)
	if strings.Contains(token.Install.Native, token.Token) || strings.Contains(token.Install.Docker, token.Token) {
		t.Fatal("install command exposed enrollment token in process arguments")
	}

	var storedHash []byte
	var fingerprint string
	if err := pool.QueryRow(context.Background(), `
		select token_hash, token_fingerprint from enrollment_tokens where id = $1
	`, token.Id).Scan(&storedHash, &fingerprint); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(storedHash, []byte(token.Token)) ||
		!bytes.Equal(storedHash, security.HashEnrollmentToken(bytes.Repeat([]byte{7}, 32), token.Token)) {
		t.Fatal("database did not contain only the keyed token hash")
	}
	if fingerprint != token.Fingerprint || fingerprint == token.Token {
		t.Fatal("database fingerprint exposed the token")
	}

	csr, privateKeyPEM := agentCSR(t)
	t.Cleanup(func() { clear(privateKeyPEM) })
	invalid := browser.request(t, http.MethodPost, "/api/v1/agent-enrollment",
		enrollRequest(token.Token, "invalid CSR", uuid.Must(uuid.NewV7())), nil)
	if invalid.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid CSR status = %d, body = %s", invalid.Code, invalid.Body.String())
	}
	var usedAt *time.Time
	if err := pool.QueryRow(context.Background(),
		"select used_at from enrollment_tokens where id = $1", token.Id).Scan(&usedAt); err != nil {
		t.Fatal(err)
	}
	if usedAt != nil {
		t.Fatal("invalid CSR consumed the token")
	}

	enrolledResponse := browser.request(t, http.MethodPost, "/api/v1/agent-enrollment",
		enrollRequest(token.Token, csr, uuid.Must(uuid.NewV7())), nil)
	if enrolledResponse.Code != http.StatusCreated {
		t.Fatalf("enrollment status = %d, body = %s", enrolledResponse.Code, enrolledResponse.Body.String())
	}
	var enrolled AgentEnrollmentResponse
	decodeResponse(t, enrolledResponse, &enrolled)
	if strings.Contains(enrolledResponse.Body.String(), string(privateKeyPEM)) {
		t.Fatal("Agent private key appeared in enrollment response")
	}
	block, _ := pem.Decode([]byte(enrolled.CertificatePem))
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	certificateAgentID, err := security.AgentIDFromCertificate(certificate)
	if err != nil || certificateAgentID != enrolled.AgentId {
		t.Fatalf("certificate identity = %v, %v", certificateAgentID, err)
	}
	oldSerial := strings.ToUpper(certificate.SerialNumber.Text(16))
	rotatedAt := now.Add(time.Hour)
	rotatedCertificateID := uuid.Must(uuid.NewV7())
	rotatedCertificate := domain.AgentCertificate{
		ID: rotatedCertificateID, AgentID: enrolled.AgentId,
		SerialNumber: "AABBCCDDEEFF", PublicKeyFingerprint: "rotated-fingerprint",
		NotBefore: rotatedAt, NotAfter: rotatedAt.Add(security.AgentCertificateValidity),
		IssuedAt: rotatedAt,
	}
	err = store.RotateAgentCertificate(
		context.Background(), enrolled.AgentId, oldSerial, rotatedCertificate,
		rotatedAt, rotatedAt.Add(24*time.Hour),
		domain.AuditEvent{
			OccurredAt: rotatedAt, ActorType: domain.ActorAgent, ActorID: enrolled.AgentId,
			Action: "AGENT_CERTIFICATE_ROTATE", ResourceType: "AGENT_CERTIFICATE",
			ResourceID: rotatedCertificateID, RequestID: uuid.Must(uuid.NewV7()),
			Result: domain.AuditSuccess, ReasonCode: "CERTIFICATE_ROTATED",
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AgentByCertificate(context.Background(), enrolled.AgentId, oldSerial, rotatedAt.Add(23*time.Hour)); err != nil {
		t.Fatalf("old certificate was rejected during overlap: %v", err)
	}
	if _, err := store.AgentByCertificate(context.Background(), enrolled.AgentId, oldSerial, rotatedAt.Add(24*time.Hour)); !errors.Is(err, domain.ErrAgentRevoked) {
		t.Fatalf("old certificate survived overlap: %v", err)
	}
	if _, err := store.AgentByCertificate(context.Background(), enrolled.AgentId, rotatedCertificate.SerialNumber, rotatedAt.Add(25*time.Hour)); err != nil {
		t.Fatalf("rotated certificate was rejected: %v", err)
	}

	duplicate := browser.request(t, http.MethodPost, "/api/v1/agent-enrollment",
		enrollRequest(token.Token, csr, uuid.Must(uuid.NewV7())), nil)
	if duplicate.Code != http.StatusForbidden {
		t.Fatalf("used token status = %d, body = %s", duplicate.Code, duplicate.Body.String())
	}
	storedAgent, err := store.Agent(context.Background(), enrolled.AgentId)
	if err != nil || storedAgent.Status != domain.AgentActive {
		t.Fatalf("stored Agent = %+v, %v", storedAgent, err)
	}
	desired, err := store.DesiredState(context.Background(), enrolled.AgentId)
	if err != nil || desired.Revision != 1 || desired.ConfigHash == "" {
		t.Fatalf("initial desired state = %+v, %v", desired, err)
	}
	var outboxCount int
	if err := pool.QueryRow(context.Background(), `
		select count(*) from outbox_events
		where aggregate_id = $1 and event_type = 'AGENT_DESIRED_STATE_CHANGED'
	`, enrolled.AgentId).Scan(&outboxCount); err != nil {
		t.Fatal(err)
	}
	if outboxCount != 1 {
		t.Fatalf("initial desired-state outbox count = %d", outboxCount)
	}
	if _, err := store.RecordAgentHeartbeat(context.Background(), domain.AgentHeartbeat{
		AgentID: enrolled.AgentId, BootID: uuid.Must(uuid.NewV7()).String(),
		UptimeSeconds: 60, AcceptedRevision: 1, StateFreeBytes: 4096,
		LocalTime: now, ReceivedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	inventory := domain.AgentInventory{
		ID: uuid.Must(uuid.NewV7()), AgentID: enrolled.AgentId, CapturedAt: now,
		Kernel: "test-kernel", OSRelease: "Test Linux", CPUArch: "amd64",
		AgentVersion: "0.2.0-test", AvailableBytes: map[string]uint64{"agent_state": 4096},
		Capabilities: []string{"desired_state_v1"},
	}
	if err := store.RecordAgentInventory(context.Background(), inventory); err != nil {
		t.Fatal(err)
	}
	agentResponse := browser.request(t, http.MethodGet,
		"/api/v1/agents/"+enrolled.AgentId.String(), nil, nil)
	if agentResponse.Code != http.StatusOK {
		t.Fatalf("Agent detail status = %d, body = %s", agentResponse.Code, agentResponse.Body.String())
	}
	var agentDetail Agent
	decodeResponse(t, agentResponse, &agentDetail)
	if agentDetail.Health != AgentHealthONLINE || agentDetail.AcceptedRevision != 1 {
		t.Fatalf("Agent health projection = %+v", agentDetail)
	}
	inventoryResponse := browser.request(t, http.MethodGet,
		"/api/v1/hosts/"+host.Id.String()+"/inventory", nil, nil)
	if inventoryResponse.Code != http.StatusOK {
		t.Fatalf("inventory status = %d, body = %s", inventoryResponse.Code, inventoryResponse.Body.String())
	}
	var inventoryDetail AgentInventory
	decodeResponse(t, inventoryResponse, &inventoryDetail)
	if inventoryDetail.AgentId != enrolled.AgentId || inventoryDetail.Kernel != "test-kernel" {
		t.Fatalf("inventory response = %+v", inventoryDetail)
	}

	updatedHost, err := store.Host(context.Background(), host.Id)
	if err != nil || updatedHost.Status != domain.HostActive {
		t.Fatalf("updated Host = %+v, %v", updatedHost, err)
	}

	revoke := browser.request(t, http.MethodPost, "/api/v1/agents/"+enrolled.AgentId.String()+"/revoke",
		AgentRevoke{Reason: "test revocation"},
		map[string]string{
			"X-CSRF-Token":    browser.cookies[csrfCookieName].Value,
			"Idempotency-Key": "revoke-agent-test",
		})
	if revoke.Code != http.StatusOK {
		t.Fatalf("revoke Agent status = %d, body = %s", revoke.Code, revoke.Body.String())
	}
	if _, err := store.AgentByCertificate(
		context.Background(), enrolled.AgentId,
		strings.ToUpper(certificate.SerialNumber.Text(16)), now,
	); !errors.Is(err, domain.ErrAgentRevoked) {
		t.Fatalf("revoked certificate was accepted: %v", err)
	}
}

func TestEnrollmentTokenIsSingleUseUnderConcurrency(t *testing.T) {
	_, _, handler, browser, _ := setupFleet(t)
	host := createTestHost(t, browser, "concurrent-edge")
	token := createTestEnrollmentToken(t, browser, host.Id)
	csrOne, _ := agentCSR(t)
	csrTwo, _ := agentCSR(t)
	requests := []AgentEnrollmentRequest{
		enrollRequest(token.Token, csrOne, uuid.Must(uuid.NewV7())),
		enrollRequest(token.Token, csrTwo, uuid.Must(uuid.NewV7())),
	}
	statuses := make(chan int, len(requests))
	var group sync.WaitGroup
	for _, body := range requests {
		group.Add(1)
		go func(body AgentEnrollmentRequest) {
			defer group.Done()
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/api/v1/agent-enrollment", encodeBody(t, body))
			request.RemoteAddr = "198.51.100.10:1234"
			request.Header.Set("Content-Type", "application/json")
			handler.ServeHTTP(recorder, request)
			statuses <- recorder.Code
		}(body)
	}
	group.Wait()
	close(statuses)
	successes := 0
	denied := 0
	for statusCode := range statuses {
		switch statusCode {
		case http.StatusCreated:
			successes++
		case http.StatusForbidden:
			denied++
		default:
			t.Fatalf("unexpected concurrent enrollment status %d", statusCode)
		}
	}
	if successes != 1 || denied != 1 {
		t.Fatalf("concurrent outcomes: success=%d denied=%d", successes, denied)
	}
}

func TestRevokedAndExpiredEnrollmentTokensAreDenied(t *testing.T) {
	_, pool, _, browser, now := setupFleet(t)
	host := createTestHost(t, browser, "token-state-edge")
	csr, _ := agentCSR(t)

	revoked := createTestEnrollmentToken(t, browser, host.Id)
	response := browser.request(
		t, http.MethodDelete, "/api/v1/enrollment-tokens/"+revoked.Id.String(), nil,
		map[string]string{"X-CSRF-Token": browser.cookies[csrfCookieName].Value},
	)
	if response.Code != http.StatusNoContent {
		t.Fatalf("revoke token status = %d", response.Code)
	}
	response = browser.request(t, http.MethodPost, "/api/v1/agent-enrollment",
		enrollRequest(revoked.Token, csr, uuid.Must(uuid.NewV7())), nil)
	if response.Code != http.StatusForbidden {
		t.Fatalf("revoked token enrollment status = %d", response.Code)
	}

	expired := createTestEnrollmentToken(t, browser, host.Id)
	if _, err := pool.Exec(context.Background(), `
		update enrollment_tokens
		set created_at = $2, expires_at = $3
		where id = $1
	`, expired.Id, now.Add(-2*time.Hour), now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	response = browser.request(t, http.MethodPost, "/api/v1/agent-enrollment",
		enrollRequest(expired.Token, csr, uuid.Must(uuid.NewV7())), nil)
	if response.Code != http.StatusForbidden {
		t.Fatalf("expired token enrollment status = %d", response.Code)
	}
}
