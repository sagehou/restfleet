package agent

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	privateKeyFile = "identity.key"
	identityFile   = "identity.json"
)

type Identity struct {
	AgentID        uuid.UUID `json:"agent_id"`
	HostID         uuid.UUID `json:"host_id"`
	CertificatePEM string    `json:"certificate_pem"`
	CABundlePEM    string    `json:"ca_bundle_pem"`
	NotAfter       time.Time `json:"not_after"`
	ServerName     string    `json:"server_name"`
	GRPCEndpoint   string    `json:"grpc_endpoint"`
}

func LoadOrCreatePrivateKey(state *State) (ed25519.PrivateKey, error) {
	path := filepath.Join(state.Directory(), privateKeyFile)
	encoded, err := os.ReadFile(path)
	if err == nil {
		return parsePrivateKey(encoded)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		return nil, err
	}
	defer clear(der)
	encoded = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	defer clear(encoded)
	if err := atomicWrite(path, encoded, 0o600); err != nil {
		return nil, err
	}
	return privateKey, nil
}

func parsePrivateKey(encoded []byte) (ed25519.PrivateKey, error) {
	block, rest := pem.Decode(encoded)
	if block == nil || block.Type != "PRIVATE KEY" || len(strings.TrimSpace(string(rest))) != 0 {
		return nil, errors.New("invalid Agent private key")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, errors.New("invalid Agent private key")
	}
	privateKey, ok := parsed.(ed25519.PrivateKey)
	if !ok {
		return nil, errors.New("agent private key is not Ed25519")
	}
	return privateKey, nil
}

func CreateCSR(privateKey ed25519.PrivateKey) ([]byte, error) {
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{},
	}, privateKey)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}), nil
}

func SaveIdentity(state *State, identity Identity) error {
	if identity.AgentID == uuid.Nil || identity.HostID == uuid.Nil ||
		identity.CertificatePEM == "" || identity.CABundlePEM == "" ||
		identity.ServerName == "" || identity.GRPCEndpoint == "" {
		return errors.New("identity bundle is incomplete")
	}
	encoded, err := json.Marshal(identity)
	if err != nil {
		return err
	}
	return atomicWrite(filepath.Join(state.Directory(), identityFile), encoded, 0o600)
}

func LoadIdentity(state *State) (Identity, error) {
	encoded, err := os.ReadFile(filepath.Join(state.Directory(), identityFile))
	if err != nil {
		return Identity{}, err
	}
	var identity Identity
	decoder := json.NewDecoder(strings.NewReader(string(encoded)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&identity); err != nil {
		return Identity{}, errors.New("invalid identity bundle")
	}
	if identity.AgentID == uuid.Nil || identity.HostID == uuid.Nil {
		return Identity{}, errors.New("invalid identity bundle")
	}
	return identity, nil
}

func TLSIdentity(state *State, identity Identity) (tls.Certificate, *x509.CertPool, error) {
	privateKeyPEM, err := os.ReadFile(filepath.Join(state.Directory(), privateKeyFile))
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	certificate, err := tls.X509KeyPair([]byte(identity.CertificatePEM), privateKeyPEM)
	clear(privateKeyPEM)
	if err != nil {
		return tls.Certificate{}, nil, errors.New("agent certificate does not match local private key")
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM([]byte(identity.CABundlePEM)) {
		return tls.Certificate{}, nil, errors.New("identity CA bundle is invalid")
	}
	return certificate, roots, nil
}

func atomicWrite(path string, value []byte, mode os.FileMode) error {
	directory := filepath.Dir(path)
	file, err := os.CreateTemp(directory, ".restfleet-identity-*")
	if err != nil {
		return err
	}
	temporary := file.Name()
	remove := true
	defer func() {
		_ = file.Close()
		if remove {
			_ = os.Remove(temporary)
		}
	}()
	if err := file.Chmod(mode); err != nil {
		return err
	}
	if _, err := file.Write(value); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporary, path); err != nil {
		return err
	}
	remove = false
	dir, err := os.Open(directory)
	if err != nil {
		return err
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil && !errors.Is(err, io.ErrClosedPipe) {
		return err
	}
	return nil
}
