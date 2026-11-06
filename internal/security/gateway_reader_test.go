package security

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sagehou/restfleet/internal/domain"
)

func readerBinding() domain.GatewayReadBinding {
	return domain.GatewayReadBinding{RepositoryID: uuid.Must(uuid.NewV7()), OperationID: uuid.Must(uuid.NewV7()), AdmissionID: uuid.Must(uuid.NewV7()), RuntimeID: uuid.Must(uuid.NewV7()), ConfigurationHash: strings.Repeat("a", 64)}
}

func TestGatewayReaderCertificateScopeLifetimeAndRole(t *testing.T) {
	now := time.Now().UTC()
	ca, key, err := NewAgentCA(now)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(key)
	binding := readerBinding()
	cert, private, err := ca.IssueGatewayReader(binding, now)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(private)
	pair, err := tls.X509KeyPair(cert, private)
	if err != nil {
		t.Fatal(err)
	}
	scope, err := GatewayReaderFromCertificate(pair.Leaf)
	if err != nil || scope.RepositoryID != binding.RepositoryID || scope.OperationID != binding.OperationID || scope.AdmissionID != binding.AdmissionID || scope.RuntimeID != binding.RuntimeID {
		t.Fatal("reader scope changed")
	}
	if _, err = pair.Leaf.Verify(x509.VerifyOptions{Roots: ca.CertPool(), KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		t.Fatal(err)
	}
	if pair.Leaf.NotAfter.After(now.Add(GatewayReaderValidity)) || pair.Leaf.NotAfter.Sub(pair.Leaf.NotBefore) > 16*time.Minute {
		t.Fatal("reader outlived its bound")
	}
	if _, err = AgentIDFromCertificate(pair.Leaf); err == nil {
		t.Fatal("reader became Agent")
	}
	for _, mutate := range []func(*x509.Certificate){
		func(c *x509.Certificate) { c.NotAfter = now.Add(-time.Second) },
		func(c *x509.Certificate) { c.NotBefore = now.Add(time.Minute) },
		func(c *x509.Certificate) { c.NotAfter = c.NotBefore.Add(time.Hour) },
		func(c *x509.Certificate) { c.Subject.CommonName = uuid.Must(uuid.NewV7()).String() },
		func(c *x509.Certificate) { c.IsCA = true },
		func(c *x509.Certificate) { c.URIs = nil },
	} {
		bad := *pair.Leaf
		mutate(&bad)
		if _, err := GatewayReaderFromCertificate(&bad); err == nil {
			t.Fatal("invalid reader certificate accepted")
		}
	}
	binding.ConfigurationHash = strings.Repeat("z", 64)
	if _, _, err = ca.IssueGatewayReader(binding, now); err == nil {
		t.Fatal("invalid configuration scope signed")
	}
}

func TestAgentCSRCannotRequestGatewayReaderRole(t *testing.T) {
	now := time.Now().UTC()
	ca, key, err := NewAgentCA(now)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(key)
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(private)
	binding := readerBinding()
	forged, _ := url.Parse(gatewayReaderPrefix + binding.RepositoryID.String() + ":" + binding.OperationID.String() + ":" + binding.AdmissionID.String() + ":" + binding.RuntimeID.String())
	raw, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: binding.OperationID.String()}, URIs: []*url.URL{forged}}, private)
	if err != nil {
		t.Fatal(err)
	}
	agent := uuid.Must(uuid.NewV7())
	issued, err := ca.IssueAgentCertificate(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: raw}), agent, now)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(issued.CertificatePEM)
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if id, err := AgentIDFromCertificate(leaf); err != nil || id != agent {
		t.Fatal("Agent identity changed")
	}
	if _, err := GatewayReaderFromCertificate(leaf); err == nil {
		t.Fatal("Agent selected reader SAN")
	}
}
