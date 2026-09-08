package domain

import (
	"bytes"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

const RepositoryCredentialsCapability = "repository_credentials_v1"

var ErrRepositoryCredential = errors.New("repository credential is unavailable or invalid")

// RepositoryCredential is borrowed secret material, never an API/trace DTO.
// The durable gateway password is NOT a BackupSession capability.
type RepositoryCredential struct {
	DeliveryID, AgentID, HostID, RepositoryID, GatewayID uuid.UUID
	Revision, GatewayRevision, ResticRevision            int64
	Endpoint                                             string
	CABundlePEM, GatewayPassword, ResticPassword         []byte
	ValidFrom                                            time.Time
}

func (RepositoryCredential) String() string   { return "[repository credential redacted]" }
func (RepositoryCredential) GoString() string { return "[repository credential redacted]" }

func (c RepositoryCredential) Clear() {
	clear(c.GatewayPassword)
	clear(c.ResticPassword)
}

func ValidGatewayOrigin(origin string) bool {
	if len(origin) > 2048 {
		return false
	}
	u, err := url.Parse(origin)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.Hostname() == "" ||
		u.User != nil || u.Path != "" || u.RawPath != "" || u.RawQuery != "" || u.ForceQuery ||
		u.Fragment != "" || u.RawFragment != "" || u.Opaque != "" || u.String() != origin ||
		strings.ContainsAny(origin, "\\\r\n\t ") || strings.Contains(origin, "#") || strings.HasSuffix(u.Host, ":") {
		return false
	}
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 || strconv.Itoa(n) != p {
			return false
		}
	}
	return true
}

func ValidCredentialCA(bundle []byte) bool {
	if len(bundle) == 0 || len(bundle) > 64<<10 {
		return false
	}
	count := 0
	for len(bytes.TrimSpace(bundle)) != 0 {
		bundle = bytes.TrimSpace(bundle)
		if !bytes.HasPrefix(bundle, []byte("-----BEGIN CERTIFICATE-----")) {
			return false
		}
		block, rest := pem.Decode(bundle)
		if block == nil || block.Type != "CERTIFICATE" || len(block.Headers) != 0 {
			return false
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil || !cert.IsCA || !cert.BasicConstraintsValid {
			return false
		}
		count++
		bundle = rest
	}
	return count > 0
}

func (c RepositoryCredential) Validate() error {
	for _, id := range []uuid.UUID{c.DeliveryID, c.AgentID, c.HostID, c.RepositoryID, c.GatewayID} {
		if id.Version() != 7 || id.Variant() != uuid.RFC4122 {
			return ErrRepositoryCredential
		}
	}
	if c.Revision < 1 || c.GatewayRevision < 1 || c.ResticRevision < 1 || c.ValidFrom.IsZero() || !ValidCredentialCA(c.CABundlePEM) {
		return ErrRepositoryCredential
	}
	path := "/restic/" + c.GatewayID.String() + "/" + c.RepositoryID.String() + "/"
	if !strings.HasSuffix(c.Endpoint, path) || !ValidGatewayOrigin(strings.TrimSuffix(c.Endpoint, path)) {
		return ErrRepositoryCredential
	}
	for _, password := range [][]byte{c.GatewayPassword, c.ResticPassword} {
		decoded, err := base64.RawURLEncoding.Strict().DecodeString(string(password))
		valid := err == nil && len(decoded) == 32 && len(password) == 43
		clear(decoded)
		if !valid {
			return ErrRepositoryCredential
		}
	}
	if bytes.Equal(c.GatewayPassword, c.ResticPassword) {
		return ErrRepositoryCredential
	}
	return nil
}

// AgentCredentialDelivery holds encrypted material and authoritative metadata.
type AgentCredentialDelivery struct {
	ID, AgentID       uuid.UUID
	Revision          int64
	ConfigurationHash string
	Repository        Repository
	Gateway, Restic   SecretEnvelope
	CreatedAt         time.Time
	AcceptedAt        *time.Time
}
