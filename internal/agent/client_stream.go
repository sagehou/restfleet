package agent

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"errors"
	"slices"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/protobuf/types/known/timestamppb"

	agentv1 "github.com/sagehou/restfleet/api/proto/gen/go/restfleet/agent/v1"
	"github.com/sagehou/restfleet/internal/domain"
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
				"certificate_rotation_v1", "desired_state_v1", "inventory_v1", domain.RepositoryCredentialsCapability, domain.RepositoryIdentityCapability,
				domain.JobResultAckCapability, domain.JobCancelCapability,
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
	resultAck := slices.Contains(welcome.GetServerCapabilities(), domain.JobResultAckCapability)

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
	reports := time.NewTicker(5 * time.Second)
	defer reports.Stop()

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
		case <-reports.C:
			if err := reportOperations(stream, &agentSequence, protocol, state, 5*time.Second, resultAck); err != nil {
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
			case message.GetCredentialRevision() != nil:
				ack, err := state.ApplyRepositoryCredential(identity, message.GetCredentialRevision())
				if err != nil {
					return true, err
				}
				if err := sendAgentMessage(stream, &agentSequence, protocol, &agentv1.AgentToServer{
					Payload: &agentv1.AgentToServer_CredentialRevisionAccepted{CredentialRevisionAccepted: ack},
				}); err != nil {
					return true, err
				}
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
			case message.GetJobDispatch() != nil:
				if err := handleJobDispatch(ctx, stream, state, config, message.GetJobDispatch(), &agentSequence, protocol); err != nil {
					return true, err
				}
			case message.GetJobCancel() != nil:
				operationID, err := uuid.Parse(message.GetJobCancel().GetOperationId())
				if err != nil {
					return true, errors.New("job cancel has an invalid operation id")
				}
				state.CancelOperation(operationID)
				if err := sendAgentMessage(stream, &agentSequence, protocol, &agentv1.AgentToServer{
					Payload: &agentv1.AgentToServer_JobCancelAccepted{JobCancelAccepted: &agentv1.JobCancelAccepted{
						OperationId: operationID.String(),
					}},
				}); err != nil {
					return true, err
				}
			case message.GetJobResultAccepted() != nil:
				operationID, err := uuid.Parse(message.GetJobResultAccepted().GetOperationId())
				if err != nil {
					return true, errors.New("job result ack has an invalid operation id")
				}
				if err := state.MarkOperationUploaded(operationID); err != nil {
					return true, err
				}
			case message.GetCertificateRotationResponse() != nil && rotationPending:
				return true, saveRotation(state, identity, message)
			default:
				// Unknown server payloads are forward-compatible and ignored.
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

// handleJobDispatch accepts one manual job, persists it before acknowledging
// and runs it in the background. Duplicate deliveries replay the stored result.
func handleJobDispatch(ctx context.Context, stream agentv1.AgentControlService_ConnectClient, state *State,
	config RunConfig, job *agentv1.JobDispatch, sequence *uint64, protocol string,
) error {
	operationID, err := uuid.Parse(job.GetOperationId())
	if err != nil {
		return errors.New("job dispatch has an invalid operation id")
	}
	planID, err := uuid.Parse(job.GetPlanId())
	if err != nil {
		return errors.New("job dispatch has an invalid plan id")
	}
	reject := func(code string) error {
		return sendAgentMessage(stream, sequence, protocol, &agentv1.AgentToServer{
			Payload: &agentv1.AgentToServer_JobRejected{JobRejected: &agentv1.JobRejected{
				JobId: job.GetJobId(), OperationId: job.GetOperationId(), ErrorCode: code,
			}},
		})
	}
	if job.GetType() != "BACKUP" {
		return reject("TYPE_UNSUPPORTED")
	}
	desired, ok, err := state.ActiveDesiredState()
	if err != nil || !ok {
		return reject("PLAN_UNAVAILABLE")
	}
	var plan domain.PlanSchedule
	found := false
	for _, candidate := range desired.Plans {
		if candidate.PlanID == planID {
			plan, found = candidate, true
			break
		}
	}
	if !found {
		return reject("PLAN_UNAVAILABLE")
	}
	deadline := job.GetDeadline().AsTime().UTC()
	if !deadline.After(time.Now()) {
		return reject("DEADLINE_EXCEEDED")
	}
	if _, exists, err := state.Operation(operationID); err == nil && exists {
		return sendAgentMessage(stream, sequence, protocol, &agentv1.AgentToServer{
			Payload: &agentv1.AgentToServer_JobAccepted{JobAccepted: &agentv1.JobAccepted{
				JobId: job.GetJobId(), OperationId: job.GetOperationId(),
			}},
		})
	}
	scheduled := job.GetNotBefore().AsTime().UTC()
	record := OperationRecord{ID: operationID, PlanID: plan.PlanID, PlanRevision: plan.Revision,
		ConfigHash: plan.ConfigHash, ScheduledAt: scheduled, StartedAt: time.Now().UTC(), Status: OperationRunning}
	if err := state.PutOperation(record); err != nil {
		return reject("PERSIST_FAILED")
	}
	if err := sendAgentMessage(stream, sequence, protocol, &agentv1.AgentToServer{
		Payload: &agentv1.AgentToServer_JobAccepted{JobAccepted: &agentv1.JobAccepted{
			JobId: job.GetJobId(), OperationId: job.GetOperationId(),
		}},
	}); err != nil {
		return err
	}
	binary := config.ResticBinary
	if binary == "" {
		binary = "restic"
	}
	executor := BackupExecutor{State: state, Binary: binary}
	runCtx, endRun := state.BeginOperation(ctx, operationID)
	go func() {
		defer endRun()
		outcome := executor.Execute(runCtx, plan, operationID, scheduled, deadline)
		record.Status = outcome.Status
		record.ExitCode = outcome.ExitCode
		record.ErrorCode = outcome.ErrorCode
		record.SnapshotID = outcome.SnapshotID
		record.FilesDone, record.TotalFiles = outcome.FilesDone, outcome.TotalFiles
		record.BytesDone, record.TotalBytes = outcome.BytesDone, outcome.TotalBytes
		record.FilesNew, record.DataAdded = outcome.FilesNew, outcome.DataAdded
		record.FinishedAt = time.Now().UTC()
		_ = state.PutOperation(record)
	}()
	return nil
}
