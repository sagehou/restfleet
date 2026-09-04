package agent

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/google/uuid"

	agentv1 "github.com/sagehou/restfleet/api/proto/gen/go/restfleet/agent/v1"
	"github.com/sagehou/restfleet/internal/security"
)

const (
	maxEnrollmentResponse = 1 << 20
	rotationWindow        = 7 * 24 * time.Hour
	stableConnection      = time.Minute
)

type EnrollConfig struct {
	ServerURL string
	TokenFile string
	CAFile    string
	Version   string
}

type enrollmentRequest struct {
	Token           string    `json:"token"`
	CSRPEM          string    `json:"csr_pem"`
	InstallID       uuid.UUID `json:"install_id"`
	AgentVersion    string    `json:"agent_version"`
	ProtocolVersion string    `json:"protocol_version"`
	Hostname        string    `json:"hostname"`
	OS              string    `json:"os"`
	Arch            string    `json:"arch"`
	Capabilities    []string  `json:"capabilities"`
}

type enrollmentResponse struct {
	AgentID        uuid.UUID `json:"agent_id"`
	HostID         uuid.UUID `json:"host_id"`
	CertificatePEM string    `json:"certificate_pem"`
	CABundlePEM    string    `json:"ca_bundle_pem"`
	NotAfter       time.Time `json:"not_after"`
	ServerName     string    `json:"server_name"`
	GRPCEndpoint   string    `json:"grpc_endpoint"`
}

func Enroll(ctx context.Context, state *State, config EnrollConfig) (Identity, error) {
	serverURL, err := url.Parse(config.ServerURL)
	if err != nil || serverURL.Scheme != "https" || serverURL.Host == "" || serverURL.User != nil {
		return Identity{}, errors.New("server must be an https URL without user info")
	}
	tokenBytes, err := os.ReadFile(config.TokenFile)
	if err != nil {
		return Identity{}, fmt.Errorf("read enrollment token: %w", err)
	}
	if len(tokenBytes) > 128 {
		clear(tokenBytes)
		return Identity{}, errors.New("enrollment token is invalid")
	}
	token := strings.TrimSpace(string(tokenBytes))
	clear(tokenBytes)
	if token == "" {
		return Identity{}, errors.New("enrollment token is invalid")
	}
	installID, err := state.InstallID()
	if err != nil {
		return Identity{}, err
	}
	privateKey, err := LoadOrCreatePrivateKey(state)
	if err != nil {
		return Identity{}, err
	}
	csr, err := CreateCSR(privateKey)
	if err != nil {
		return Identity{}, err
	}
	hostname, err := os.Hostname()
	if err != nil {
		return Identity{}, err
	}
	body, err := json.Marshal(enrollmentRequest{
		Token: token, CSRPEM: string(csr), InstallID: installID,
		AgentVersion: config.Version, ProtocolVersion: "1.0",
		Hostname: hostname, OS: runtime.GOOS, Arch: runtime.GOARCH,
		Capabilities: []string{"certificate_rotation_v1"},
	})
	token = ""
	if err != nil {
		return Identity{}, err
	}
	defer clear(body)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	if config.CAFile != "" {
		roots, err := rootsFromFile(config.CAFile)
		if err != nil {
			return Identity{}, err
		}
		transport.TLSClientConfig.RootCAs = roots
	}
	endpoint := strings.TrimRight(serverURL.String(), "/") + "/api/v1/agent-enrollment"
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return Identity{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	response, err := (&http.Client{Transport: transport, Timeout: 30 * time.Second}).Do(request)
	if err != nil {
		return Identity{}, errors.New("agent enrollment request failed")
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, maxEnrollmentResponse+1))
	if err != nil {
		return Identity{}, errors.New("read Agent enrollment response")
	}
	if len(responseBody) > maxEnrollmentResponse {
		return Identity{}, errors.New("agent enrollment response is too large")
	}
	if response.StatusCode != http.StatusCreated {
		return Identity{}, fmt.Errorf("agent enrollment denied with HTTP %d", response.StatusCode)
	}
	var enrolled enrollmentResponse
	decoder := json.NewDecoder(bytes.NewReader(responseBody))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&enrolled); err != nil {
		return Identity{}, errors.New("invalid Agent enrollment response")
	}
	identity := Identity(enrolled)
	if err := validateIdentityMaterial(state, identity); err != nil {
		return Identity{}, err
	}
	if err := SaveIdentity(state, identity); err != nil {
		return Identity{}, err
	}
	return identity, nil
}

type RunConfig struct {
	Version string
}

func Run(ctx context.Context, state *State, config RunConfig) error {
	delay := time.Second
	random := rand.New(rand.NewSource(time.Now().UnixNano()))
	for {
		startedAt := time.Now()
		welcomed, _ := connectOnce(ctx, state, config)
		if ctx.Err() != nil {
			return nil
		}
		if welcomed && time.Since(startedAt) >= stableConnection {
			delay = time.Second
		}
		wait := time.Duration(random.Int63n(int64(delay) + 1))
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
		if delay < 5*time.Minute {
			delay *= 2
			if delay > 5*time.Minute {
				delay = 5 * time.Minute
			}
		}
	}
}

func saveRotation(state *State, identity Identity, response *agentv1.ServerToAgent) error {
	rotated := response.GetCertificateRotationResponse()
	if rotated == nil || rotated.GetNotAfter() == nil || rotated.GetNotAfter().CheckValid() != nil {
		return errors.New("invalid certificate rotation response")
	}
	identity.CertificatePEM = rotated.GetCertificatePem()
	identity.CABundlePEM = rotated.GetCaBundlePem()
	identity.NotAfter = rotated.GetNotAfter().AsTime()
	if err := validateIdentityMaterial(state, identity); err != nil {
		return err
	}
	return SaveIdentity(state, identity)
}

func validateIdentityMaterial(state *State, identity Identity) error {
	certificate, _, err := TLSIdentity(state, identity)
	if err != nil || len(certificate.Certificate) == 0 {
		return errors.New("invalid Agent identity material")
	}
	leaf, err := x509.ParseCertificate(certificate.Certificate[0])
	if err != nil {
		return errors.New("invalid Agent identity material")
	}
	agentID, err := security.AgentIDFromCertificate(leaf)
	if err != nil || agentID != identity.AgentID || !leaf.NotAfter.Equal(identity.NotAfter) {
		return errors.New("invalid Agent identity material")
	}
	return nil
}

func rootsFromFile(path string) (*x509.CertPool, error) {
	value, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(value) {
		return nil, errors.New("CA file does not contain a PEM certificate")
	}
	return roots, nil
}

func readBootID() string {
	value, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(value))
}
