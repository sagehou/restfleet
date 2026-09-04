package agentgrpc

import (
	"context"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	agentv1 "github.com/sagehou/restfleet/api/proto/gen/go/restfleet/agent/v1"
	"github.com/sagehou/restfleet/internal/security"
	control "github.com/sagehou/restfleet/internal/server"
)

const (
	protocolCurrent   = "1.0"
	protocolPrevious  = "0.9"
	maxPendingResults = 100
)

type Service struct {
	agentv1.UnimplementedAgentControlServiceServer
	control          *control.ControlPlane
	caBundlePEM      []byte
	heartbeatSeconds uint32
	mu               sync.Mutex
	connections      map[uuid.UUID]map[chan struct{}]struct{}
}

func New(controlPlane *control.ControlPlane, caBundlePEM []byte, heartbeat time.Duration) *Service {
	return &Service{
		control: controlPlane, caBundlePEM: append([]byte(nil), caBundlePEM...),
		heartbeatSeconds: uint32(heartbeat.Seconds()),
		connections:      make(map[uuid.UUID]map[chan struct{}]struct{}),
	}
}

func (s *Service) DisconnectAgent(agentID uuid.UUID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for revoked := range s.connections[agentID] {
		close(revoked)
	}
	delete(s.connections, agentID)
}

func (s *Service) Connect(
	stream agentv1.AgentControlService_ConnectServer,
) error {
	certificate, meta, err := peerCertificate(stream.Context())
	if err != nil {
		return s.denied(stream.Context(), "MTLS_IDENTITY_INVALID", meta, codes.Unauthenticated, "valid Agent client certificate required")
	}
	agentID, err := security.AgentIDFromCertificate(certificate)
	if err != nil {
		return s.denied(stream.Context(), "CERTIFICATE_IDENTITY_INVALID", meta, codes.Unauthenticated, "valid Agent identity required")
	}
	serial := strings.ToUpper(certificate.SerialNumber.Text(16))
	agent, err := s.control.AgentByCertificate(stream.Context(), agentID, serial, time.Now().UTC())
	if err != nil {
		return s.denied(stream.Context(), "AGENT_REVOKED_OR_CERTIFICATE_INVALID", meta, codes.PermissionDenied, "Agent identity is not active")
	}

	revoked := s.register(agentID)
	defer s.unregister(agentID, revoked)
	received := receive(stream)
	first, err := waitForMessage(stream, received, revoked)
	if err != nil {
		return err
	}
	hello := first.GetHello()
	if hello == nil {
		return status.Error(codes.InvalidArgument, "Hello must be the first message")
	}
	if err := validateEnvelope(first.GetMessageId(), first.GetProtocolVersion(), first.GetSentAt()); err != nil {
		return status.Error(codes.InvalidArgument, "invalid message envelope")
	}
	installID, err := uuid.Parse(hello.GetInstallId())
	if err != nil || installID != agent.InstallID {
		return s.denied(stream.Context(), "INSTALL_ID_MISMATCH", meta, codes.PermissionDenied, "Agent identity mismatch")
	}
	selected := selectProtocol(hello.GetSupportedProtocolVersions())
	if selected == "" || len(hello.GetPendingResultIds()) > maxPendingResults {
		return status.Error(codes.FailedPrecondition, "Agent protocol is incompatible")
	}
	if err := s.control.MarkAgentConnected(
		stream.Context(), agentID, installID, hello.GetAgentVersion(), selected,
		agent.Hostname, hello.GetBootId(), hello.GetResticVersion(),
	); err != nil {
		return status.Error(codes.PermissionDenied, "Agent identity is not active")
	}
	connectionID, err := uuid.NewV7()
	if err != nil {
		return status.Error(codes.Internal, "connection initialization failed")
	}
	if err := stream.Send(&agentv1.ServerToAgent{
		MessageId:       newMessageID(),
		ProtocolVersion: selected,
		SentAt:          timestamppb.Now(),
		Sequence:        1,
		Payload: &agentv1.ServerToAgent_Welcome{Welcome: &agentv1.Welcome{
			ConnectionId: connectionID.String(), SelectedProtocolVersion: selected,
			ServerTime: timestamppb.Now(), HeartbeatIntervalSeconds: s.heartbeatSeconds,
			DesiredConfigRevision: agent.DesiredRevision, MinimumAgentVersion: "0.1.0",
		}},
	}); err != nil {
		return err
	}

	message, err := waitForMessage(stream, received, revoked)
	if err != nil {
		return err
	}
	if err := validateEnvelope(message.GetMessageId(), message.GetProtocolVersion(), message.GetSentAt()); err != nil {
		return status.Error(codes.InvalidArgument, "invalid message envelope")
	}
	rotation := message.GetCertificateRotationRequest()
	if rotation == nil {
		return status.Error(codes.InvalidArgument, "unsupported Agent message")
	}
	meta.RequestID, _ = uuid.Parse(message.GetMessageId())
	issued, err := s.control.RotateAgentCertificate(
		stream.Context(), agentID, serial, []byte(rotation.GetCsrPem()), meta,
	)
	if errors.Is(err, control.ErrInvalidEnrollment) {
		return status.Error(codes.InvalidArgument, "invalid certificate rotation request")
	}
	if err != nil {
		return status.Error(codes.PermissionDenied, "certificate rotation denied")
	}
	if err := stream.Send(&agentv1.ServerToAgent{
		MessageId: newMessageID(), ProtocolVersion: selected,
		SentAt: timestamppb.Now(), Sequence: message.GetSequence() + 1,
		Payload: &agentv1.ServerToAgent_CertificateRotationResponse{
			CertificateRotationResponse: &agentv1.CertificateRotationResponse{
				CertificatePem: string(issued.CertificatePEM),
				CaBundlePem:    string(s.caBundlePEM),
				NotAfter:       timestamppb.New(issued.NotAfter),
			},
		},
	}); err != nil {
		return err
	}
	return nil
}

type receiveResult struct {
	message *agentv1.AgentToServer
	err     error
}

func receive(
	stream agentv1.AgentControlService_ConnectServer,
) <-chan receiveResult {
	results := make(chan receiveResult, 1)
	go func() {
		for {
			message, err := stream.Recv()
			select {
			case results <- receiveResult{message: message, err: err}:
			case <-stream.Context().Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	return results
}

func waitForMessage(
	stream agentv1.AgentControlService_ConnectServer,
	received <-chan receiveResult,
	revoked <-chan struct{},
) (*agentv1.AgentToServer, error) {
	select {
	case <-revoked:
		return nil, status.Error(codes.PermissionDenied, "Agent identity revoked")
	case <-stream.Context().Done():
		return nil, stream.Context().Err()
	case result := <-received:
		if errors.Is(result.err, io.EOF) {
			return nil, io.EOF
		}
		return result.message, result.err
	}
}

func (s *Service) register(agentID uuid.UUID) chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	revoked := make(chan struct{})
	if s.connections[agentID] == nil {
		s.connections[agentID] = make(map[chan struct{}]struct{})
	}
	s.connections[agentID][revoked] = struct{}{}
	return revoked
}

func (s *Service) unregister(agentID uuid.UUID, revoked chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	connections := s.connections[agentID]
	if connections == nil {
		return
	}
	delete(connections, revoked)
	if len(connections) == 0 {
		delete(s.connections, agentID)
	}
}

func (s *Service) denied(
	ctx context.Context,
	reason string,
	meta control.RequestMeta,
	code codes.Code,
	message string,
) error {
	if err := s.control.RecordDenied(ctx, "AGENT_CONNECT", "AGENT", reason, meta); err != nil {
		return status.Error(codes.Unavailable, "agent authorization audit unavailable")
	}
	return status.Error(code, message)
}

func peerCertificate(ctx context.Context) (*x509.Certificate, control.RequestMeta, error) {
	meta := control.RequestMeta{}
	requestID, err := uuid.NewV7()
	if err != nil {
		return nil, meta, err
	}
	meta.RequestID = requestID
	peerInfo, ok := peer.FromContext(ctx)
	if !ok {
		return nil, meta, errors.New("gRPC peer is missing")
	}
	host := peerInfo.Addr.String()
	if parsed, _, err := net.SplitHostPort(host); err == nil {
		host = parsed
	}
	meta.SourceIPHash = security.HashSecret(host)
	tlsInfo, ok := peerInfo.AuthInfo.(credentials.TLSInfo)
	if !ok || len(tlsInfo.State.PeerCertificates) == 0 {
		return nil, meta, errors.New("mTLS client certificate is missing")
	}
	return tlsInfo.State.PeerCertificates[0], meta, nil
}

func selectProtocol(supported []string) string {
	for _, candidate := range []string{protocolCurrent, protocolPrevious} {
		for _, version := range supported {
			if version == candidate {
				return candidate
			}
		}
	}
	return ""
}

func validateEnvelope(messageID, protocolVersion string, sentAt *timestamppb.Timestamp) error {
	id, err := uuid.Parse(messageID)
	if err != nil || id.Version() != 7 || (protocolVersion != protocolCurrent && protocolVersion != protocolPrevious) {
		return errors.New("invalid envelope")
	}
	if sentAt == nil || sentAt.CheckValid() != nil {
		return errors.New("invalid envelope timestamp")
	}
	return nil
}

func newMessageID() string {
	id, err := uuid.NewV7()
	if err != nil {
		return uuid.Nil.String()
	}
	return id.String()
}
