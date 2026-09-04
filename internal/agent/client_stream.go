package agent

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"errors"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/protobuf/types/known/timestamppb"

	agentv1 "github.com/sagehou/restfleet/api/proto/gen/go/restfleet/agent/v1"
)

func connectOnce(ctx context.Context, state *State, config RunConfig) (bool, error) {
	identity, err := LoadIdentity(state)
	if err != nil {
		return false, err
	}
	installID, err := state.InstallID()
	if err != nil {
		return false, err
	}
	acceptedRevision, err := state.AcceptedRevision()
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
		grpc.MaxCallRecvMsgSize(maxEnrollmentResponse),
		grpc.MaxCallSendMsgSize(maxEnrollmentResponse),
	))
	if err != nil {
		return false, err
	}
	defer connection.Close()
	stream, err := agentv1.NewAgentControlServiceClient(connection).Connect(ctx)
	if err != nil {
		return false, err
	}
	bootID := readBootID()
	var agentSequence uint64
	if err := sendAgentMessage(stream, &agentSequence, "1.0", &agentv1.AgentToServer{
		Payload: &agentv1.AgentToServer_Hello{Hello: &agentv1.Hello{
			InstallId: installID.String(), BootId: bootID, AgentVersion: config.Version,
			SupportedProtocolVersions: []string{"1.0", "0.9"},
			AcceptedConfigRevision:    acceptedRevision,
			Capabilities: []string{
				"certificate_rotation_v1", "desired_state_v1", "inventory_v1",
			},
			LocalTime: timestamppb.Now(),
		}},
	}); err != nil {
		return false, err
	}
	response, err := stream.Recv()
	if err != nil {
		return false, err
	}
	welcome := response.GetWelcome()
	if err := validateWelcome(response, welcome, acceptedRevision); err != nil {
		return false, err
	}
	protocol := welcome.GetSelectedProtocolVersion()
	serverSequence := response.GetSequence()
	clockOffset := welcome.GetServerTime().AsTime().Sub(time.Now().UTC())

	if pending, ok, err := state.PendingConfigResult(); err != nil {
		return true, err
	} else if ok {
		if err := sendConfigResult(stream, &agentSequence, protocol, pending); err != nil {
			return true, err
		}
		if err := state.ClearPendingConfigResult(pending.Revision); err != nil {
			return true, err
		}
	}
	if err := sendAgentMessage(stream, &agentSequence, protocol, &agentv1.AgentToServer{
		Payload: &agentv1.AgentToServer_InventoryReport{
			InventoryReport: inventorySnapshot(state, config.Version, clockOffset),
		},
	}); err != nil {
		return true, err
	}

	heartbeatEvery := time.Duration(welcome.GetHeartbeatIntervalSeconds()) * time.Second
	heartbeats := time.NewTicker(heartbeatEvery)
	defer heartbeats.Stop()
	rotateAfter := time.Until(identity.NotAfter.Add(-rotationWindow))
	if rotateAfter < 0 {
		rotateAfter = 0
	}
	rotationTimer := time.NewTimer(rotateAfter)
	defer rotationTimer.Stop()
	incoming := receiveServerMessages(ctx, stream)
	rotationPending := false

	for {
		select {
		case <-ctx.Done():
			return true, nil
		case <-heartbeats.C:
			acceptedRevision, err = state.AcceptedRevision()
			if err != nil {
				return true, err
			}
			if err := sendAgentMessage(stream, &agentSequence, protocol, &agentv1.AgentToServer{
				Payload: &agentv1.AgentToServer_Heartbeat{
					Heartbeat: heartbeatSnapshot(state, bootID, acceptedRevision, clockOffset),
				},
			}); err != nil {
				return true, err
			}
		case <-rotationTimer.C:
			privateKey, err := LoadOrCreatePrivateKey(state)
			if err != nil {
				return true, err
			}
			if err := sendRotation(stream, &agentSequence, protocol, privateKey); err != nil {
				return true, err
			}
			rotationPending = true
		case received := <-incoming:
			if received.err != nil {
				return true, received.err
			}
			message := received.message
			if err := validateServerEnvelope(message, protocol); err != nil {
				return true, err
			}
			if message.GetSequence() <= serverSequence {
				continue
			}
			if message.GetSequence() != serverSequence+1 {
				return true, errors.New("server message sequence gap")
			}
			serverSequence = message.GetSequence()
			switch {
			case message.GetDesiredStateSnapshot() != nil:
				result, err := state.ApplyDesiredState(identity.AgentID, message.GetDesiredStateSnapshot())
				if err != nil {
					return true, err
				}
				if err := sendConfigResult(stream, &agentSequence, protocol, result); err != nil {
					return true, err
				}
				if err := state.ClearPendingConfigResult(result.Revision); err != nil {
					return true, err
				}
			case message.GetCertificateRotationResponse() != nil && rotationPending:
				return true, saveRotation(state, identity, message)
			default:
				return true, errors.New("unsupported Server message")
			}
		}
	}
}

type serverReceive struct {
	message *agentv1.ServerToAgent
	err     error
}

func receiveServerMessages(
	ctx context.Context,
	stream agentv1.AgentControlService_ConnectClient,
) <-chan serverReceive {
	incoming := make(chan serverReceive, 1)
	go func() {
		defer close(incoming)
		for {
			message, err := stream.Recv()
			select {
			case incoming <- serverReceive{message: message, err: err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	return incoming
}

func sendAgentMessage(
	stream agentv1.AgentControlService_ConnectClient,
	sequence *uint64,
	protocol string,
	message *agentv1.AgentToServer,
) error {
	messageID, err := uuid.NewV7()
	if err != nil {
		return err
	}
	*sequence++
	message.MessageId = messageID.String()
	message.ProtocolVersion = protocol
	message.SentAt = timestamppb.Now()
	message.Sequence = *sequence
	return stream.Send(message)
}

func sendConfigResult(
	stream agentv1.AgentControlService_ConnectClient,
	sequence *uint64,
	protocol string,
	result PendingConfigResult,
) error {
	if err := result.Validate(); err != nil {
		return err
	}
	message := &agentv1.AgentToServer{}
	if result.Accepted {
		message.Payload = &agentv1.AgentToServer_ConfigAccepted{
			ConfigAccepted: &agentv1.ConfigAccepted{
				Revision: result.Revision, ConfigHash: result.ConfigHash,
			},
		}
	} else {
		message.Payload = &agentv1.AgentToServer_ConfigRejected{
			ConfigRejected: &agentv1.ConfigRejected{
				Revision: result.Revision, ErrorCode: result.ErrorCode,
				FieldPath: result.FieldPath,
			},
		}
	}
	return sendAgentMessage(stream, sequence, protocol, message)
}

func sendRotation(
	stream agentv1.AgentControlService_ConnectClient,
	sequence *uint64,
	protocol string,
	privateKey ed25519.PrivateKey,
) error {
	csr, err := CreateCSR(privateKey)
	if err != nil {
		return err
	}
	return sendAgentMessage(stream, sequence, protocol, &agentv1.AgentToServer{
		Payload: &agentv1.AgentToServer_CertificateRotationRequest{
			CertificateRotationRequest: &agentv1.CertificateRotationRequest{CsrPem: string(csr)},
		},
	})
}

func validateWelcome(
	message *agentv1.ServerToAgent,
	welcome *agentv1.Welcome,
	acceptedRevision int64,
) error {
	if welcome == nil || welcome.GetServerTime() == nil ||
		welcome.GetServerTime().CheckValid() != nil ||
		welcome.GetHeartbeatIntervalSeconds() < 5 ||
		welcome.GetHeartbeatIntervalSeconds() > 300 ||
		welcome.GetDesiredConfigRevision() < acceptedRevision ||
		(message.GetProtocolVersion() != "1.0" && message.GetProtocolVersion() != "0.9") ||
		welcome.GetSelectedProtocolVersion() != message.GetProtocolVersion() ||
		message.GetSequence() != 1 {
		return errors.New("invalid Welcome")
	}
	return validateServerEnvelope(message, welcome.GetSelectedProtocolVersion())
}

func validateServerEnvelope(message *agentv1.ServerToAgent, protocol string) error {
	if message == nil || message.GetProtocolVersion() != protocol ||
		message.GetSentAt() == nil || message.GetSentAt().CheckValid() != nil {
		return errors.New("invalid Server message envelope")
	}
	id, err := uuid.Parse(message.GetMessageId())
	if err != nil || id.Version() != 7 {
		return errors.New("invalid Server message envelope")
	}
	return nil
}
