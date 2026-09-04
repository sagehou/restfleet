package agent

import (
	"bytes"
	"context"
	"crypto/ed25519"
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
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/protobuf/types/known/timestamppb"

	agentv1 "github.com/sagehou/restfleet/api/proto/gen/go/restfleet/agent/v1"
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

func connectOnce(ctx context.Context, state *State, config RunConfig) (bool, error) {
	identity, err := LoadIdentity(state)
	if err != nil {
		return false, err
	}
	installID, err := state.InstallID()
	if err != nil {
		return false, err
	}
	certificate, roots, err := TLSIdentity(state, identity)
	if err != nil {
		return false, err
	}
	connection, err := grpc.NewClient(identity.GRPCEndpoint, grpc.WithTransportCredentials(
		credentials.NewTLS(&tls.Config{
			MinVersion: tls.VersionTLS12, ServerName: identity.ServerName,
			RootCAs: roots, Certificates: []tls.Certificate{certificate},
		}),
	), grpc.WithDefaultCallOptions(
		grpc.MaxCallRecvMsgSize(maxEnrollmentResponse), grpc.MaxCallSendMsgSize(maxEnrollmentResponse),
	))
	if err != nil {
		return false, err
	}
	defer connection.Close()
	stream, err := agentv1.NewAgentControlServiceClient(connection).Connect(ctx)
	if err != nil {
		return false, err
	}
	messageID, err := uuid.NewV7()
	if err != nil {
		return false, err
	}
	if err := stream.Send(&agentv1.AgentToServer{
		MessageId: messageID.String(), ProtocolVersion: "1.0",
		SentAt: timestamppb.Now(), Sequence: 1,
		Payload: &agentv1.AgentToServer_Hello{Hello: &agentv1.Hello{
			InstallId: installID.String(), BootId: readBootID(), AgentVersion: config.Version,
			SupportedProtocolVersions: []string{"1.0", "0.9"},
			Capabilities:              []string{"certificate_rotation_v1"},
			LocalTime:                 timestamppb.Now(),
		}},
	}); err != nil {
		return false, err
	}
	response, err := stream.Recv()
	if err != nil {
		return false, err
	}
	if response.GetWelcome() == nil {
		return false, errors.New("server did not send Welcome")
	}

	rotateAt := identity.NotAfter.Add(-rotationWindow)
	delay := time.Until(rotateAt)
	if delay < 0 {
		delay = 0
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	incoming := make(chan serverReceive, 1)
	go func() {
		message, err := stream.Recv()
		incoming <- serverReceive{message: message, err: err}
	}()
	select {
	case <-ctx.Done():
		return true, nil
	case result := <-incoming:
		if result.err == nil {
			result.err = errors.New("unexpected Server message")
		}
		return true, result.err
	case <-timer.C:
	}
	privateKey, err := LoadOrCreatePrivateKey(state)
	if err != nil {
		return true, err
	}
	if err := sendRotation(stream, privateKey); err != nil {
		return true, err
	}
	select {
	case <-ctx.Done():
		return true, nil
	case result := <-incoming:
		if result.err != nil {
			return true, result.err
		}
		return true, saveRotation(state, identity, result.message)
	}
}

type serverReceive struct {
	message *agentv1.ServerToAgent
	err     error
}

func sendRotation(
	stream agentv1.AgentControlService_ConnectClient,
	privateKey ed25519.PrivateKey,
) error {
	csr, err := CreateCSR(privateKey)
	if err != nil {
		return err
	}
	messageID, err := uuid.NewV7()
	if err != nil {
		return err
	}
	return stream.Send(&agentv1.AgentToServer{
		MessageId: messageID.String(), ProtocolVersion: "1.0",
		SentAt: timestamppb.Now(), Sequence: 2,
		Payload: &agentv1.AgentToServer_CertificateRotationRequest{
			CertificateRotationRequest: &agentv1.CertificateRotationRequest{CsrPem: string(csr)},
		},
	})
}

func saveRotation(state *State, identity Identity, response *agentv1.ServerToAgent) error {
	rotated := response.GetCertificateRotationResponse()
	if rotated == nil || rotated.GetNotAfter() == nil || rotated.GetNotAfter().CheckValid() != nil {
		return errors.New("invalid certificate rotation response")
	}
	identity.CertificatePEM = rotated.GetCertificatePem()
	identity.CABundlePEM = rotated.GetCaBundlePem()
	identity.NotAfter = rotated.GetNotAfter().AsTime()
	return SaveIdentity(state, identity)
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
