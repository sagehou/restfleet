package httpapi

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	agentv1 "github.com/sagehou/restfleet/api/proto/gen/go/restfleet/agent/v1"
	"github.com/sagehou/restfleet/internal/agent"
	"github.com/sagehou/restfleet/internal/server/agentgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func credentialServerTLS(t *testing.T) (tls.Certificate, []byte) {
	t.Helper()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "localhost"}, DNSNames: []string{"localhost"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, pub, key)
	if err != nil {
		t.Fatal(err)
	}
	cert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(keyDER)
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	defer clear(keyPEM)
	pair, err := tls.X509KeyPair(cert, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	return pair, cert
}

func TestAgentCredentialMTLSNativeClientAndLegacyCapability(t *testing.T) {
	store, pool, _, b, _ := setupFleet(t)
	s, err := agent.OpenState(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	installID, err := s.InstallID()
	if err != nil {
		t.Fatal(err)
	}
	privateKey, err := agent.LoadOrCreatePrivateKey(s)
	if err != nil {
		t.Fatal(err)
	}
	csr, err := agent.CreateCSR(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	clear(privateKey)
	host := createTestHost(t, b, "mTLS credential Host")
	credential := createCredential(t, b, "mTLS credential")
	repo := createTestRepository(t, b, host.Id, credential.Id, "mTLS repository")
	initializeDeliveryRepository(t, store, b, repo)
	token := createTestEnrollmentToken(t, b, host.Id)
	response := b.request(t, http.MethodPost, "/api/v1/agent-enrollment", enrollRequest(token.Token, string(csr), installID), nil)
	if response.Code != http.StatusCreated {
		t.Fatal("enrollment failed")
	}
	var enrolled AgentEnrollmentResponse
	decodeResponse(t, response, &enrolled)
	pair, serverCA := credentialServerTLS(t)
	clientCAs := x509.NewCertPool()
	if !clientCAs.AppendCertsFromPEM([]byte(enrolled.CaBundlePem)) {
		t.Fatal("client trust invalid")
	}
	c := deliveryControl(t, store, serverCA, "https://gateway.example")
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{pair}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: clientCAs})), grpc.MaxRecvMsgSize(1<<20), grpc.MaxSendMsgSize(1<<20))
	agentv1.RegisterAgentControlServiceServer(server, agentgrpc.New(c, serverCA, 15*time.Second))
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = server.Serve(listener) }()
	defer server.Stop()
	identity := agent.Identity{AgentID: enrolled.AgentId, HostID: host.Id, CertificatePEM: enrolled.CertificatePem, CABundlePEM: string(serverCA),
		NotAfter: enrolled.NotAfter, ServerName: "localhost", GRPCEndpoint: listener.Addr().String()}
	if err := agent.SaveIdentity(s, identity); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	done := make(chan error, 1)
	go func() { done <- agent.Run(ctx, s, agent.RunConfig{Version: "0.4.0-test"}) }()
	defer func() { cancel(); <-done }()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		var accepted bool
		if err := pool.QueryRow(ctx, "select exists(select 1 from repository_agent_deliveries where agent_id=$1 and accepted_at is not null)", enrolled.AgentId).Scan(&accepted); err != nil {
			t.Fatal("ACK query failed")
		}
		if accepted {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("native client did not persist and ACK credentials")
		case <-ticker.C:
		}
	}
	value, ok, err := s.LoadRepositoryCredential(identity)
	if err != nil || !ok || value.RepositoryID != repo.Id {
		t.Fatal("ACK preceded credential persistence")
	}
	value.Clear()
	// A 1.0/0.9 peer without the optional capability must never see the new secret message.
	certificate, roots, err := agent.TLSIdentity(s, identity)
	if err != nil {
		t.Fatal(err)
	}
	connection, err := grpc.NewClient(identity.GRPCEndpoint, grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12, ServerName: "localhost", RootCAs: roots, Certificates: []tls.Certificate{certificate}})))
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	legacyCtx, stop := context.WithTimeout(ctx, 500*time.Millisecond)
	defer stop()
	stream, err := agentv1.NewAgentControlServiceClient(connection).Connect(legacyCtx)
	if err != nil {
		t.Fatal(err)
	}
	err = stream.Send(&agentv1.AgentToServer{MessageId: uuid.Must(uuid.NewV7()).String(), ProtocolVersion: "0.9", Sequence: 1, SentAt: timestamppb.Now(),
		Payload: &agentv1.AgentToServer_Hello{Hello: &agentv1.Hello{InstallId: installID.String(), BootId: "legacy-boot", AgentVersion: "legacy-test", SupportedProtocolVersions: []string{"0.9"}, AcceptedConfigRevision: 1, LocalTime: timestamppb.Now()}}})
	if err != nil {
		t.Fatal(err)
	}
	welcome, err := stream.Recv()
	if err != nil || welcome.GetWelcome() == nil {
		t.Fatal("legacy peer could not connect")
	}
	if _, err := stream.Recv(); status.Code(err) != codes.DeadlineExceeded {
		t.Fatal("legacy peer received an unnegotiated message")
	}
}
