package security

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/sagehou/restfleet/internal/domain"
)

const GatewayReaderValidity = 15 * time.Minute
const gatewayReaderPrefix = "urn:restfleet:repository-reader:"

// IssueGatewayReader creates a new central key. Agent CSR issuance cannot select
// this SAN; the certificate is never delivered to an Agent or Gateway owner.
func (c *AgentCA) IssueGatewayReader(binding domain.GatewayReadBinding, now time.Time) (certificate, key []byte, err error) {
	if c == nil || !binding.Valid() {
		return nil, nil, errors.New("invalid gateway reader scope")
	}
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	defer clear(private)
	serial, err := randomSerial()
	if err != nil {
		return nil, nil, err
	}
	identity, _ := url.Parse(gatewayReaderPrefix + binding.RepositoryID.String() + ":" + binding.OperationID.String() + ":" + binding.AdmissionID.String() + ":" + binding.RuntimeID.String())
	template := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: binding.OperationID.String()}, URIs: []*url.URL{identity},
		NotBefore: now.UTC().Add(-time.Minute), NotAfter: now.UTC().Add(GatewayReaderValidity), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, c.certificate, public, c.privateKey)
	if err != nil {
		return nil, nil, err
	}
	rawKey, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		return nil, nil, err
	}
	defer clear(rawKey)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: rawKey}), nil
}

func GatewayReaderFromCertificate(leaf *x509.Certificate) (domain.GatewayReadBinding, error) {
	var binding domain.GatewayReadBinding
	if leaf == nil || len(leaf.URIs) != 1 || leaf.IsCA || time.Now().Before(leaf.NotBefore) || !time.Now().Before(leaf.NotAfter) ||
		leaf.NotAfter.Sub(leaf.NotBefore) > GatewayReaderValidity+time.Minute {
		return binding, errors.New("invalid gateway reader certificate")
	}
	raw := leaf.URIs[0].String()
	if !strings.HasPrefix(raw, gatewayReaderPrefix) {
		return binding, errors.New("invalid gateway reader identity")
	}
	parts := strings.Split(strings.TrimPrefix(raw, gatewayReaderPrefix), ":")
	if len(parts) != 4 {
		return binding, errors.New("invalid gateway reader identity")
	}
	ids := []*uuid.UUID{&binding.RepositoryID, &binding.OperationID, &binding.AdmissionID, &binding.RuntimeID}
	for i, target := range ids {
		id, err := uuid.Parse(parts[i])
		if err != nil || id.String() != parts[i] || id.Version() != 7 || id.Variant() != uuid.RFC4122 {
			return binding, errors.New("invalid gateway reader scope")
		}
		*target = id
	}
	if leaf.Subject.CommonName != binding.OperationID.String() {
		return binding, errors.New("invalid gateway reader identity")
	}
	return binding, nil
}
