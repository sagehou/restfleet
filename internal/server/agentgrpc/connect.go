package agentgrpc

import (
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	agentv1 "github.com/sagehou/restfleet/api/proto/gen/go/restfleet/agent/v1"
	"github.com/sagehou/restfleet/internal/domain"
	"github.com/sagehou/restfleet/internal/security"
	control "github.com/sagehou/restfleet/internal/server"
)

func (s *Service) Connect(stream agentv1.AgentControlService_ConnectServer) error {
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
	if hello == nil || first.GetSequence() != 1 ||
		hello.GetLocalTime() == nil || hello.GetLocalTime().CheckValid() != nil {
		return s.invalidMessage(stream, meta, "HELLO_INVALID")
	}
	if err := validateEnvelope(first.GetMessageId(), first.GetProtocolVersion(), first.GetSentAt()); err != nil {
		return s.invalidMessage(stream, meta, "MESSAGE_ENVELOPE_INVALID")
	}
	installID, err := uuid.Parse(hello.GetInstallId())
	if err != nil || installID != agent.InstallID {
		return s.denied(stream.Context(), "INSTALL_ID_MISMATCH", meta, codes.PermissionDenied, "Agent identity mismatch")
	}
	selected := selectProtocol(hello.GetSupportedProtocolVersions())
	if selected == "" || len(hello.GetPendingResultIds()) > maxPendingResults {
		return s.denied(stream.Context(), "PROTOCOL_INCOMPATIBLE", meta, codes.FailedPrecondition, "Agent protocol is incompatible")
	}
	if hello.GetAcceptedConfigRevision() < 0 ||
		hello.GetAcceptedConfigRevision() > agent.DesiredRevision {
		return s.invalidMessage(stream, meta, "ACCEPTED_REVISION_INVALID")
	}
	agent, err = s.control.MarkAgentConnected(
		stream.Context(), agentID, installID, hello.GetAgentVersion(), selected,
		agent.Hostname, hello.GetBootId(), hello.GetResticVersion(),
		hello.GetAcceptedConfigRevision(),
	)
	if err != nil {
		return s.denied(stream.Context(), "AGENT_CONNECTION_UPDATE_DENIED", meta, codes.PermissionDenied, "Agent identity is not active")
	}
	connectionID, err := uuid.NewV7()
	if err != nil {
		return status.Error(codes.Internal, "connection initialization failed")
	}
	var serverSequence uint64
	if err := sendServerMessage(stream, &serverSequence, selected, &agentv1.ServerToAgent{
		Payload: &agentv1.ServerToAgent_Welcome{Welcome: &agentv1.Welcome{
			ConnectionId: connectionID.String(), SelectedProtocolVersion: selected,
			ServerTime: timestamppb.Now(), HeartbeatIntervalSeconds: s.heartbeatSeconds,
			DesiredConfigRevision: agent.DesiredRevision, MinimumAgentVersion: "0.1.0",
		}},
	}); err != nil {
		return err
	}
	if hello.GetAcceptedConfigRevision() < agent.DesiredRevision {
		if err := s.sendDesiredState(stream, agentID, selected, &serverSequence); err != nil {
			return err
		}
	}

	credentialCapable := slices.Contains(hello.GetCapabilities(), domain.RepositoryCredentialsCapability)
	identityCapable := slices.Contains(hello.GetCapabilities(), domain.RepositoryIdentityCapability)
	if credentialCapable {
		if err := s.sendAgentCredential(stream, agentID, selected, &serverSequence, true, identityCapable); err != nil {
			return err
		}
	}
	lastAgentSequence := first.GetSequence()
	for {
		message, err := waitForMessage(stream, received, revoked)
		if err != nil {
			return err
		}
		if err := validateEnvelope(message.GetMessageId(), message.GetProtocolVersion(), message.GetSentAt()); err != nil ||
			message.GetProtocolVersion() != selected {
			return s.invalidMessage(stream, meta, "MESSAGE_ENVELOPE_INVALID")
		}
		if message.GetSequence() <= lastAgentSequence {
			s.observeHeartbeatResult("duplicate")
			continue
		}
		if message.GetSequence() != lastAgentSequence+1 {
			return s.invalidMessage(stream, meta, "MESSAGE_SEQUENCE_INVALID")
		}
		lastAgentSequence = message.GetSequence()
		meta.RequestID, _ = uuid.Parse(message.GetMessageId())
		switch {
		case message.GetHeartbeat() != nil:
			heartbeat, err := heartbeatFromProto(agentID, message.GetHeartbeat())
			if err != nil {
				s.observeHeartbeatResult("rejected")
				return s.invalidMessage(stream, meta, "HEARTBEAT_INVALID")
			}
			if _, err := s.control.RecordAgentHeartbeat(stream.Context(), heartbeat); err != nil {
				s.observeHeartbeatResult("rejected")
				var validation *control.ValidationError
				if errors.As(err, &validation) {
					return s.invalidMessage(stream, meta, "HEARTBEAT_INVALID")
				}
				if errors.Is(err, domain.ErrAgentRevoked) {
					return s.denied(stream.Context(), "AGENT_REVOKED", meta, codes.PermissionDenied, "Agent identity is not active")
				}
				return status.Error(codes.Unavailable, "heartbeat persistence failed")
			}
			s.observeHeartbeatResult("accepted")
			if credentialCapable {
				if err := s.sendAgentCredential(stream, agentID, selected, &serverSequence, false, identityCapable); err != nil {
					return err
				}
			}
			if err := s.dispatchQueuedJob(stream, agentID, selected, &serverSequence); err != nil {
				return err
			}
		case message.GetInventoryReport() != nil:
			inventory, err := inventoryFromProto(agentID, message.GetInventoryReport())
			if err != nil {
				return s.invalidMessage(stream, meta, "INVENTORY_INVALID")
			}
			if err := s.control.RecordAgentInventory(stream.Context(), inventory); err != nil {
				var validation *control.ValidationError
				if errors.As(err, &validation) {
					return s.invalidMessage(stream, meta, "INVENTORY_INVALID")
				}
				if errors.Is(err, domain.ErrAgentRevoked) {
					return s.denied(stream.Context(), "AGENT_REVOKED", meta, codes.PermissionDenied, "Agent identity is not active")
				}
				return status.Error(codes.Unavailable, "inventory persistence failed")
			}
		case message.GetCredentialRevisionAccepted() != nil:
			ack := message.GetCredentialRevisionAccepted()
			id, err := uuid.Parse(ack.GetDeliveryId())
			if !credentialCapable || err != nil || s.control.AcceptAgentCredential(stream.Context(), agentID, id, ack.GetRevision()) != nil {
				return s.invalidMessage(stream, meta, "CREDENTIAL_ACK_INVALID")
			}
		case message.GetJobProgress() != nil:
			progress := message.GetJobProgress()
			operationID, opErr := uuid.Parse(progress.GetOperationId())
			planID, planErr := uuid.Parse(progress.GetPlanId())
			if opErr != nil || planErr != nil || progress.GetScheduledAt() == nil || progress.GetObservedAt() == nil {
				return s.invalidMessage(stream, meta, "JOB_PROGRESS_INVALID")
			}
			if err := s.control.RecordOperationProgress(stream.Context(), domain.BackupOperationProgress{
				OperationID: operationID, PlanID: planID, AgentID: agentID,
				PlanRevision: progress.GetPlanRevision(), ScheduledAt: progress.GetScheduledAt().AsTime().UTC(),
				Sequence: progress.GetSequence(), Stage: progress.GetStage(),
				FilesDone: progress.GetFilesDone(), TotalFiles: progress.GetTotalFiles(),
				BytesDone: progress.GetBytesDone(), TotalBytes: progress.GetTotalBytes(),
				ObservedAt: progress.GetObservedAt().AsTime().UTC(),
			}); err != nil {
				return status.Error(codes.Unavailable, "job progress persistence failed")
			}
		case message.GetJobResult() != nil:
			result := message.GetJobResult()
			operationID, opErr := uuid.Parse(result.GetOperationId())
			planID, planErr := uuid.Parse(result.GetPlanId())
			if opErr != nil || planErr != nil || result.GetScheduledAt() == nil || result.GetFinishedAt() == nil {
				return s.invalidMessage(stream, meta, "JOB_RESULT_INVALID")
			}
			if err := s.control.RecordOperationResult(stream.Context(), domain.BackupOperationResult{
				OperationID: operationID, PlanID: planID, AgentID: agentID,
				PlanRevision: result.GetPlanRevision(), ScheduledAt: result.GetScheduledAt().AsTime().UTC(),
				Status: result.GetStatus(), ErrorCode: result.GetErrorCode(), ExitCode: int(result.GetExitCode()),
				SnapshotID: result.GetSnapshotId(), FilesDone: result.GetFilesNew(), BytesDone: result.GetDataAdded(),
				FinishedAt: result.GetFinishedAt().AsTime().UTC(),
			}); err != nil {
				var validation *control.ValidationError
				if errors.As(err, &validation) {
					return s.invalidMessage(stream, meta, "JOB_RESULT_INVALID")
				}
				return status.Error(codes.Unavailable, "job result persistence failed")
			}
		case message.GetConfigAccepted() != nil:
			accepted := message.GetConfigAccepted()
			if err := s.control.AcceptAgentConfig(stream.Context(), agentID, domain.AgentConfigResult{
				Revision: accepted.GetRevision(), ConfigHash: accepted.GetConfigHash(),
			}); err != nil {
				return s.invalidMessage(stream, meta, "CONFIG_ACCEPTED_INVALID")
			}
		case message.GetConfigRejected() != nil:
			rejected := message.GetConfigRejected()
			if err := s.control.RejectAgentConfig(stream.Context(), agentID, domain.AgentConfigResult{
				Revision: rejected.GetRevision(), ErrorCode: rejected.GetErrorCode(),
				FieldPath: rejected.GetFieldPath(),
			}); err != nil {
				return s.invalidMessage(stream, meta, "CONFIG_REJECTED_INVALID")
			}
		case message.GetCertificateRotationRequest() != nil:
			rotation := message.GetCertificateRotationRequest()
			issued, err := s.control.RotateAgentCertificate(
				stream.Context(), agentID, serial, []byte(rotation.GetCsrPem()), meta,
			)
			if errors.Is(err, control.ErrInvalidEnrollment) {
				return status.Error(codes.InvalidArgument, "invalid certificate rotation request")
			}
			if err != nil {
				return status.Error(codes.PermissionDenied, "certificate rotation denied")
			}
			if err := sendServerMessage(stream, &serverSequence, selected, &agentv1.ServerToAgent{
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
		default:
			return s.invalidMessage(stream, meta, "MESSAGE_PAYLOAD_UNSUPPORTED")
		}
	}
}

func (s *Service) sendDesiredState(
	stream agentv1.AgentControlService_ConnectServer,
	agentID uuid.UUID,
	protocol string,
	sequence *uint64,
) error {
	state, err := s.control.DesiredState(stream.Context(), agentID)
	if err != nil {
		return status.Error(codes.Unavailable, "desired state unavailable")
	}
	plans := make([]*agentv1.PlanSchedule, 0, len(state.Plans))
	for _, plan := range state.Plans {
		plans = append(plans, &agentv1.PlanSchedule{
			PlanId: plan.PlanID.String(), Revision: plan.Revision, Name: plan.Name,
			HostId: plan.HostID.String(), RepositoryId: plan.RepositoryID.String(),
			Cron: plan.Cron, Timezone: plan.Timezone,
			JitterSeconds: int32(plan.JitterSeconds), MisfireGraceSeconds: int32(plan.MisfireGraceSeconds),
			ConcurrencyPolicy: plan.ConcurrencyPolicy, Paths: plan.Paths,
			ExcludePatterns: plan.ExcludePatterns, ExcludeCaches: plan.ExcludeCaches,
			OneFileSystem: plan.OneFileSystem, ConfigHash: plan.ConfigHash,
		})
	}
	if err := sendServerMessage(stream, sequence, protocol, &agentv1.ServerToAgent{
		Payload: &agentv1.ServerToAgent_DesiredStateSnapshot{
			DesiredStateSnapshot: &agentv1.DesiredStateSnapshot{
				Revision: state.Revision, GeneratedAt: timestamppb.New(state.GeneratedAt),
				ConfigHash: state.ConfigHash,
				RuntimePolicy: &agentv1.RuntimePolicy{
					MaxParallelIoJobs: state.RuntimePolicy.MaxParallelIOJobs,
					LogLimitBytes:     state.RuntimePolicy.LogLimitBytes,
				},
				Plans: plans,
			},
		},
	}); err != nil {
		return err
	}
	if err := s.control.MarkDesiredStatePublished(stream.Context(), agentID, state.Revision); err != nil {
		return status.Error(codes.Unavailable, "desired state dispatch acknowledgement failed")
	}
	return nil
}

func sendServerMessage(
	stream agentv1.AgentControlService_ConnectServer,
	sequence *uint64,
	protocol string,
	message *agentv1.ServerToAgent,
) error {
	*sequence++
	message.MessageId = newMessageID()
	message.ProtocolVersion = protocol
	message.SentAt = timestamppb.Now()
	message.Sequence = *sequence
	return stream.Send(message)
}

func heartbeatFromProto(
	agentID uuid.UUID,
	message *agentv1.Heartbeat,
) (domain.AgentHeartbeat, error) {
	if message.GetUptimeSeconds() > uint64(^uint64(0)>>1) ||
		message.GetStateFreeBytes() > uint64(^uint64(0)>>1) ||
		message.GetLocalTime() == nil || message.GetLocalTime().CheckValid() != nil ||
		len(message.GetActiveOperations()) > 0 || len(message.GetNextRuns()) > 0 {
		return domain.AgentHeartbeat{}, errors.New("invalid heartbeat")
	}
	checks := make([]domain.AgentHealthCheck, 0, len(message.GetHealthChecks()))
	for _, check := range message.GetHealthChecks() {
		if check == nil {
			return domain.AgentHeartbeat{}, errors.New("invalid heartbeat")
		}
		checks = append(checks, domain.AgentHealthCheck{
			Name: check.GetName(), Healthy: check.GetHealthy(), ErrorCode: check.GetErrorCode(),
		})
	}
	return domain.AgentHeartbeat{
		AgentID: agentID, BootID: message.GetBootId(),
		UptimeSeconds:    int64(message.GetUptimeSeconds()),
		AcceptedRevision: message.GetAcceptedRevision(),
		ResticVersion:    message.GetResticVersion(),
		StateFreeBytes:   int64(message.GetStateFreeBytes()),
		ClockOffsetMS:    message.GetClockOffsetMs(), LocalTime: message.GetLocalTime().AsTime(),
		HealthChecks: checks,
	}, nil
}

func inventoryFromProto(
	agentID uuid.UUID,
	message *agentv1.InventoryReport,
) (domain.AgentInventory, error) {
	if message.GetCapturedAt() == nil || message.GetCapturedAt().CheckValid() != nil {
		return domain.AgentInventory{}, errors.New("invalid inventory")
	}
	available := make(map[string]uint64, len(message.GetAvailableBytes()))
	for name, value := range message.GetAvailableBytes() {
		available[name] = value
	}
	return domain.AgentInventory{
		AgentID: agentID, CapturedAt: message.GetCapturedAt().AsTime().UTC(),
		Kernel: message.GetKernel(), OSRelease: message.GetOsRelease(),
		CPUArch: message.GetCpuArch(), AgentVersion: message.GetAgentVersion(),
		ResticVersion: message.GetResticVersion(), Containerized: message.GetContainerized(),
		AvailableBytes: available, ClockOffsetMS: message.GetClockOffsetMs(),
		Capabilities: append([]string(nil), message.GetCapabilities()...),
	}, nil
}

func (s *Service) invalidMessage(
	stream agentv1.AgentControlService_ConnectServer,
	meta control.RequestMeta,
	reason string,
) error {
	return s.denied(stream.Context(), reason, meta, codes.InvalidArgument, "invalid Agent message")
}

func (s *Service) observeHeartbeatResult(result string) {
	if s.observeHeartbeat != nil {
		s.observeHeartbeat(result)
	}
}

// dispatchQueuedJob pushes at most one manual job per heartbeat. Delivery is
// at-least-once; the Agent de-duplicates by operation id.
func (s *Service) dispatchQueuedJob(stream agentv1.AgentControlService_ConnectServer, agentID uuid.UUID, protocol string, sequence *uint64) error {
	operation, ok, err := s.control.NextQueuedJob(stream.Context(), agentID)
	if err != nil || !ok {
		return nil
	}
	scheduled := timestamppb.Now()
	if operation.ScheduledAt != nil {
		scheduled = timestamppb.New(*operation.ScheduledAt)
	}
	if err := sendServerMessage(stream, sequence, protocol, &agentv1.ServerToAgent{
		Payload: &agentv1.ServerToAgent_JobDispatch{JobDispatch: &agentv1.JobDispatch{
			JobId: operation.ID.String(), OperationId: operation.ID.String(), Type: operation.Type,
			CreatedAt: timestamppb.New(operation.CreatedAt), NotBefore: scheduled,
			Deadline: timestamppb.New(operation.CreatedAt.Add(24 * time.Hour)),
			PlanId:   operation.PlanID.String(), PlanRevision: operation.PlanRevision, ConfigHash: operation.ConfigHash,
		}},
	}); err != nil {
		return err
	}
	return s.control.MarkJobDispatched(stream.Context(), operation.ID)
}
