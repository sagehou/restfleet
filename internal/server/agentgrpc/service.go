package agentgrpc

import (
	"context"
	"crypto/x509"
	"errors"
	"io"
	"net"
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
	observeHeartbeat func(string)
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

func (s *Service) SetHeartbeatObserver(observe func(string)) {
	s.observeHeartbeat = observe
}

func (s *Service) DisconnectAgent(agentID uuid.UUID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for revoked := range s.connections[agentID] {
		close(revoked)
	}
	delete(s.connections, agentID)
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
