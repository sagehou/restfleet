package restic

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sagehou/restfleet/internal/domain"
	"github.com/sagehou/restfleet/internal/security"
)

func gatewayReaderServer(t *testing.T, handler http.Handler) (*httptest.Server, GatewayReadAccess) {
	t.Helper()
	ca, key, err := security.NewAgentCA(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	defer clear(key)
	block, _ := pem.Decode(key)
	private, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	block, _ = pem.Decode(ca.CertificatePEM())
	parent, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "gateway-test"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}, parent, parent.PublicKey, private)
	if err != nil {
		t.Fatal(err)
	}
	serverPair, err := tls.X509KeyPair(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert}), key)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(handler)
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{serverPair}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: ca.CertPool()}
	server.StartTLS()
	t.Cleanup(server.Close)
	binding := domain.GatewayReadBinding{RepositoryID: uuid.Must(uuid.NewV7()), OperationID: uuid.Must(uuid.NewV7()), AdmissionID: uuid.Must(uuid.NewV7()), RuntimeID: uuid.Must(uuid.NewV7()), ConfigurationHash: strings.Repeat("a", 64)}
	clientCert, clientKey, err := ca.IssueGatewayReader(binding, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { clear(clientKey) })
	return server, GatewayReadAccess{Origin: server.URL, Binding: binding, CertificatePEM: clientCert, PrivateKeyPEM: clientKey, CABundlePEM: ca.CertificatePEM()}
}

func TestGatewayReadAdapterPollsValidatesTLSAndCleansPrivateFiles(t *testing.T) {
	gateway := uuid.Must(uuid.NewV7())
	session := uuid.Must(uuid.NewV7())
	var posts, gets, deletes atomic.Int32
	server, access := gatewayReaderServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 {
			t.Error("reader escaped mTLS")
			return
		}
		reader, err := security.GatewayReaderFromCertificate(r.TLS.PeerCertificates[0])
		if err != nil {
			t.Error("reader role missing")
			return
		}
		expected := "/gateway/v1/repositories/" + reader.RepositoryID.String() + "/read-sessions/" + reader.OperationID.String()
		if r.URL.Path != expected {
			t.Error("reader escaped control route")
		}
		state := "STARTING"
		switch r.Method {
		case http.MethodPost:
			posts.Add(1)
		case http.MethodGet:
			gets.Add(1)
			state = "ACTIVE"
		case http.MethodDelete:
			deletes.Add(1)
			w.WriteHeader(204)
			return
		default:
			t.Error("unexpected method")
		}
		_ = json.NewEncoder(w).Encode(gatewayReadSession{SessionID: session, State: state, EndpointPath: "/restic/" + gateway.String() + "/" + reader.RepositoryID.String() + "/" + session.String() + "/", ExpiresAt: time.Now().UTC().Add(time.Minute)})
	}))
	scope := access.Binding
	password := []byte(strings.Repeat("p", 43))
	var directory string
	called := false
	p := &Provisioner{} // Any attempt to start a second runtime would fail.
	err := p.withGatewayRepository(context.Background(), access, gateway, scope.RepositoryID, password, func(_ context.Context, dir, repository, passwordFile string) error {
		called = true
		directory = dir
		if repository != "rest:"+server.URL+"/restic/"+gateway.String()+"/"+scope.RepositoryID.String()+"/"+session.String()+"/" {
			t.Fatal("unexpected repository")
		}
		for _, name := range []string{"password", "client.pem", "ca.pem"} {
			info, err := os.Lstat(filepath.Join(dir, name))
			if err != nil || info.Mode().Perm() != 0600 {
				t.Fatal("unsafe central read file")
			}
		}
		info, err := os.Stat(dir)
		if err != nil || info.Mode().Perm() != 0700 {
			t.Fatal("unsafe central read directory")
		}
		raw, err := os.ReadFile(passwordFile)
		if err != nil || string(raw) != string(password) {
			t.Fatal("password was not supplied through file")
		}
		clear(raw)
		return nil
	})
	if err != nil || !called || posts.Load() != 1 || gets.Load() != 1 || deletes.Load() != 1 {
		t.Fatal("read lifecycle did not complete")
	}
	if _, err = os.Lstat(directory); !os.IsNotExist(err) {
		t.Fatal("read material survived cleanup")
	}
}

func TestGatewayReadAdapterRejectsForgedRoutesAndAlwaysClosesAmbiguousSession(t *testing.T) {
	for _, failure := range []string{"cross-repository", "redirect", "malformed", "callback"} {
		t.Run(failure, func(t *testing.T) {
			gateway := uuid.Must(uuid.NewV7())
			session := uuid.Must(uuid.NewV7())
			var closed atomic.Int32
			_, access := gatewayReaderServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodDelete {
					closed.Add(1)
					w.WriteHeader(204)
					return
				}
				if failure == "redirect" {
					w.Header().Set("Location", "https://untrusted.invalid/")
					w.WriteHeader(307)
					return
				}
				if failure == "malformed" {
					_, _ = w.Write([]byte("private-provider-error"))
					return
				}
				reader, err := security.GatewayReaderFromCertificate(r.TLS.PeerCertificates[0])
				if err != nil {
					t.Error(err)
					return
				}
				repository := reader.RepositoryID
				if failure == "cross-repository" {
					repository = uuid.Must(uuid.NewV7())
				}
				_ = json.NewEncoder(w).Encode(gatewayReadSession{SessionID: session, State: "ACTIVE", EndpointPath: "/restic/" + gateway.String() + "/" + repository.String() + "/" + session.String() + "/", ExpiresAt: time.Now().UTC().Add(time.Minute)})
			}))
			scope := access.Binding
			called := false
			err := (&Provisioner{}).withGatewayRepository(context.Background(), access, gateway, scope.RepositoryID, []byte(strings.Repeat("p", 43)), func(context.Context, string, string, string) error { called = true; return ErrProcessFailed })
			if err == nil || strings.Contains(err.Error(), "private-provider-error") || closed.Load() != 1 || (failure != "callback" && called) {
				t.Fatal("unsafe reader response accepted or ambiguous session leaked")
			}
		})
	}
}

func TestGatewayReadAdapterRejectsWrongTrustBeforeCallback(t *testing.T) {
	_, access := gatewayReaderServer(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("untrusted request reached server") }))
	ca, key, err := security.NewAgentCA(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	defer clear(key)
	access.CABundlePEM = ca.CertificatePEM()
	called := false
	err = (&Provisioner{}).withGatewayRepository(context.Background(), access, uuid.Must(uuid.NewV7()), access.Binding.RepositoryID, []byte(strings.Repeat("p", 43)), func(context.Context, string, string, string) error { called = true; return nil })
	if !errors.Is(err, ErrBackendFailed) || called {
		t.Fatal("wrong TLS trust bypassed")
	}
}
