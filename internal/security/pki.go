package security

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"math/big"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
)

const AgentCertificateValidity = 30 * 24 * time.Hour

type AgentCA struct {
	certificate    *x509.Certificate
	certificatePEM []byte
	privateKey     ed25519.PrivateKey
}

type IssuedAgentCertificate struct {
	CertificatePEM       []byte
	SerialNumber         string
	PublicKeyFingerprint string
	NotBefore            time.Time
	NotAfter             time.Time
}

func NewAgentCA(now time.Time) (*AgentCA, []byte, error) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, nil, err
	}
	now = now.UTC()
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "RestFleet Agent CA"},
		NotBefore:             now.Add(-5 * time.Minute),
		NotAfter:              now.Add(10 * 365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		IsCA:                  true,
		BasicConstraintsValid: true,
		MaxPathLenZero:        true,
	}
	certificateDER, err := x509.CreateCertificate(rand.Reader, template, template, publicKey, privateKey)
	if err != nil {
		return nil, nil, err
	}
	certificatePEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificateDER})
	privateDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		return nil, nil, err
	}
	privatePEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER})
	ca, err := LoadAgentCA(certificatePEM, privatePEM)
	clear(privateDER)
	return ca, privatePEM, err
}

func LoadAgentCA(certificatePEM, privateKeyPEM []byte) (*AgentCA, error) {
	certificateBlock, certificateRest := pem.Decode(certificatePEM)
	if certificateBlock == nil || certificateBlock.Type != "CERTIFICATE" || len(strings.TrimSpace(string(certificateRest))) != 0 {
		return nil, errors.New("invalid agent CA certificate")
	}
	certificate, err := x509.ParseCertificate(certificateBlock.Bytes)
	if err != nil || !certificate.IsCA {
		return nil, errors.New("invalid agent CA certificate")
	}
	keyBlock, keyRest := pem.Decode(privateKeyPEM)
	if keyBlock == nil || keyBlock.Type != "PRIVATE KEY" || len(strings.TrimSpace(string(keyRest))) != 0 {
		return nil, errors.New("invalid agent CA private key")
	}
	parsedKey, err := x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, errors.New("invalid agent CA private key")
	}
	privateKey, ok := parsedKey.(ed25519.PrivateKey)
	publicKey, publicOK := certificate.PublicKey.(ed25519.PublicKey)
	if !ok || !publicOK || !publicKey.Equal(privateKey.Public()) {
		return nil, errors.New("agent CA key does not match certificate")
	}
	return &AgentCA{
		certificate: certificate, certificatePEM: append([]byte(nil), certificatePEM...),
		privateKey: append(ed25519.PrivateKey(nil), privateKey...),
	}, nil
}

func (c *AgentCA) CertificatePEM() []byte {
	return append([]byte(nil), c.certificatePEM...)
}

func (c *AgentCA) CertPool() *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(c.certificate)
	return pool
}

func (c *AgentCA) IssueAgentCertificate(csrPEM []byte, agentID uuid.UUID, now time.Time) (IssuedAgentCertificate, error) {
	request, err := ParseAgentCSR(csrPEM)
	if err != nil {
		return IssuedAgentCertificate{}, err
	}
	serial, err := randomSerial()
	if err != nil {
		return IssuedAgentCertificate{}, err
	}
	now = now.UTC()
	notBefore := now.Add(-5 * time.Minute)
	notAfter := now.Add(AgentCertificateValidity)
	identityURI, _ := url.Parse(AgentIdentityURI(agentID))
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: agentID.String()},
		URIs:         []*url.URL{identityURI},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, c.certificate, request.PublicKey, c.privateKey)
	if err != nil {
		return IssuedAgentCertificate{}, err
	}
	spki, err := x509.MarshalPKIXPublicKey(request.PublicKey)
	if err != nil {
		return IssuedAgentCertificate{}, err
	}
	fingerprint := sha256.Sum256(spki)
	return IssuedAgentCertificate{
		CertificatePEM:       pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		SerialNumber:         strings.ToUpper(serial.Text(16)),
		PublicKeyFingerprint: hex.EncodeToString(fingerprint[:]),
		NotBefore:            notBefore, NotAfter: notAfter,
	}, nil
}

func ParseAgentCSR(csrPEM []byte) (*x509.CertificateRequest, error) {
	block, rest := pem.Decode(csrPEM)
	if block == nil || block.Type != "CERTIFICATE REQUEST" || len(strings.TrimSpace(string(rest))) != 0 {
		return nil, errors.New("invalid CSR")
	}
	request, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil || request.CheckSignature() != nil {
		return nil, errors.New("invalid CSR signature")
	}
	if _, ok := request.PublicKey.(ed25519.PublicKey); !ok {
		return nil, errors.New("CSR key must be Ed25519")
	}
	return request, nil
}

func AgentIdentityURI(agentID uuid.UUID) string {
	return "urn:restfleet:agent:" + agentID.String()
}

func AgentIDFromCertificate(certificate *x509.Certificate) (uuid.UUID, error) {
	if certificate == nil || len(certificate.URIs) != 1 {
		return uuid.Nil, errors.New("agent certificate identity is missing")
	}
	const prefix = "urn:restfleet:agent:"
	identity := certificate.URIs[0].String()
	if !strings.HasPrefix(identity, prefix) {
		return uuid.Nil, errors.New("agent certificate identity is invalid")
	}
	id, err := uuid.Parse(strings.TrimPrefix(identity, prefix))
	if err != nil || certificate.Subject.CommonName != id.String() {
		return uuid.Nil, errors.New("agent certificate identity is invalid")
	}
	return id, nil
}

func randomSerial() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return nil, err
	}
	if serial.Sign() == 0 {
		return big.NewInt(1), nil
	}
	return serial, nil
}
