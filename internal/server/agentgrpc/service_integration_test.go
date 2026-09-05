package agentgrpc

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	agentv1 "github.com/sagehou/restfleet/api/proto/gen/go/restfleet/agent/v1"
	"github.com/sagehou/restfleet/internal/domain"
	"github.com/sagehou/restfleet/internal/security"
	control "github.com/sagehou/restfleet/internal/server"
)

type grpcTestStore struct {
	control.Store
	mu        sync.Mutex
	agent     domain.Agent
	revoked   bool
	audits    []domain.AuditEvent
	desired   domain.DesiredState
	published int
}

func (s *grpcTestStore) AgentByCertificate(
	_ context.Context,
	id uuid.UUID,
	serial string,
	_ time.Time,
) (domain.Agent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.revoked || id != s.agent.ID || serial != s.agent.CertificateSerial {
		return domain.Agent{}, domain.ErrAgentRevoked
	}
	return s.agent, nil
}

func (s *grpcTestStore) MarkAgentConnected(
	_ context.Context,
	id, installID uuid.UUID,
	_, _, _, _, _ string,
	acceptedRevision int64,
	_ time.Time,
) (domain.Agent, error) {
	if id != s.agent.ID || installID != s.agent.InstallID ||
		acceptedRevision > s.agent.DesiredRevision {
		return domain.Agent{}, domain.ErrAgentRevoked
	}
	s.agent.AcceptedRevision = acceptedRevision
	return s.agent, nil
}

func (s *grpcTestStore) DesiredState(
	_ context.Context,
	agentID uuid.UUID,
) (domain.DesiredState, error) {
	if agentID != s.desired.AgentID {
		return domain.DesiredState{}, domain.ErrNotFound
	}
	return s.desired, nil
}

func (s *grpcTestStore) MarkDesiredStatePublished(
	_ context.Context,
	agentID uuid.UUID,
	revision int64,
	_ time.Time,
) error {
	if agentID != s.desired.AgentID || revision != s.desired.Revision {
		return domain.ErrNotFound
	}
	s.published++
	return nil
}

func (s *grpcTestStore) RecordAudit(_ context.Context, event domain.AuditEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.audits = append(s.audits, event)
	return nil
}

func TestMTLSHelloAndRevocationGate(t *testing.T) {
	now := time.Now().UTC()
	agentCA, caPrivatePEM, err := security.NewAgentCA(now)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { clear(caPrivatePEM) })
	_, agentPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, agentPrivate)
	if err != nil {
		t.Fatal(err)
	}
	csrPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER})
	agentID := uuid.Must(uuid.NewV7())
	issued, err := agentCA.IssueAgentCertificate(csrPEM, agentID, now)
	if err != nil {
		t.Fatal(err)
	}
	agentPrivateDER, err := x509.MarshalPKCS8PrivateKey(agentPrivate)
	if err != nil {
		t.Fatal(err)
	}
	agentPrivatePEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: agentPrivateDER})
	clear(agentPrivateDER)
	clientCertificate, err := tls.X509KeyPair(issued.CertificatePEM, agentPrivatePEM)
	clear(agentPrivatePEM)
	if err != nil {
		t.Fatal(err)
	}

	serverCertificate, serverRoots := testServerCertificate(t, now)
	store := &grpcTestStore{agent: domain.Agent{
		ID: agentID, InstallID: uuid.Must(uuid.NewV7()),
		CertificateSerial: issued.SerialNumber, Status: domain.AgentActive,
		Hostname: "agent-test",
	}}
	controlPlane, err := control.NewControlPlane(store, control.Settings{
		BootstrapToken: "unused",
		PasswordParams: security.Argon2Params{
			Memory: 64, Iterations: 1, Parallelism: 1, SaltLength: 8, KeyLength: 16,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	service := New(controlPlane, serverRoots, 15*time.Second)
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{serverCertificate},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    agentCA.CertPool(),
	})))
	agentv1.RegisterAgentControlServiceServer(server, service)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() {
		server.Stop()
		_ = listener.Close()
	})

	connection, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(
		credentials.NewTLS(&tls.Config{
			MinVersion: tls.VersionTLS12, ServerName: "localhost",
			RootCAs:      certificatePool(t, serverRoots),
			Certificates: []tls.Certificate{clientCertificate},
		}),
	))
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client := agentv1.NewAgentControlServiceClient(connection)
	stream, err := client.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(testHello(t, store.agent.InstallID)); err != nil {
		t.Fatal(err)
	}
	response, err := stream.Recv()
	if err != nil || response.GetWelcome() == nil {
		t.Fatalf("Welcome = %v, %v", response, err)
	}

	service.DisconnectAgent(agentID)
	if _, err := stream.Recv(); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("active stream after revoke = %v", err)
	}
	store.mu.Lock()
	store.revoked = true
	store.mu.Unlock()

	for _, waitForRejection := range []bool{false, true} {
		rejected, err := client.Connect(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if waitForRejection {
			// Force the server-first ordering without sleeps. Header is only a
			// synchronization point; Recv below must still prove the final status.
			_, _ = rejected.Header()
		}
		// gRPC Send may return EOF when the server rejects before Hello is sent;
		// only Recv exposes the authoritative RPC status. Neither EOF nor a
		// successful send is evidence that revoked credentials were accepted.
		if err := rejected.Send(testHello(t, store.agent.InstallID)); err != nil && !errors.Is(err, io.EOF) {
			t.Fatal(err)
		}
		if _, err := rejected.Recv(); status.Code(err) != codes.PermissionDenied {
			t.Fatalf("new stream with revoked Agent = %v", err)
		}
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.audits) == 0 || store.audits[len(store.audits)-1].Result != domain.AuditDenied {
		t.Fatal("revoked connection attempt was not audited")
	}
}

func testHello(t *testing.T, installID uuid.UUID) *agentv1.AgentToServer {
	t.Helper()
	id := uuid.Must(uuid.NewV7())
	return &agentv1.AgentToServer{
		MessageId: id.String(), ProtocolVersion: "1.0",
		SentAt: timestamppb.Now(), Sequence: 1,
		Payload: &agentv1.AgentToServer_Hello{Hello: &agentv1.Hello{
			InstallId: installID.String(), AgentVersion: "test",
			SupportedProtocolVersions: []string{"1.0"},
			LocalTime:                 timestamppb.Now(),
		}},
	}
}

func testServerCertificate(t *testing.T, now time.Time) (tls.Certificate, []byte) {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		DNSNames:     []string{"localhost"},
		NotBefore:    now.Add(-time.Minute), NotAfter: now.Add(time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA:        true, BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, publicKey, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	certificatePEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	privateDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	privatePEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER})
	clear(privateDER)
	certificate, err := tls.X509KeyPair(certificatePEM, privatePEM)
	clear(privatePEM)
	if err != nil {
		t.Fatal(err)
	}
	return certificate, certificatePEM
}

func certificatePool(t *testing.T, certificatePEM []byte) *x509.CertPool {
	t.Helper()
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(certificatePEM) {
		t.Fatal(errors.New("failed to build test certificate pool"))
	}
	return pool
}
