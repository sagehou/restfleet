package security

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestAgentCertificateIgnoresCSRIdentity(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	ca, privatePEM, err := NewAgentCA(now)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { clear(privatePEM) })
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	forged, _ := url.Parse("urn:restfleet:agent:00000000-0000-0000-0000-000000000001")
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: "forged"},
		URIs:    []*url.URL{forged},
	}, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	csrPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER})
	agentID := uuid.MustParse("0198f1da-2c57-7d3b-9c92-6e2f05293643")
	issued, err := ca.IssueAgentCertificate(csrPEM, agentID, now)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(issued.CertificatePEM)
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	got, err := AgentIDFromCertificate(certificate)
	if err != nil || got != agentID {
		t.Fatalf("certificate identity = %v, %v", got, err)
	}
	if len(certificate.URIs) != 1 || certificate.URIs[0].String() == forged.String() {
		t.Fatal("CSR-provided SAN was trusted")
	}
}

func TestAgentCSRRejectsRSA(t *testing.T) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseAgentCSR(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER})); err == nil {
		t.Fatal("RSA CSR was accepted")
	}
}
