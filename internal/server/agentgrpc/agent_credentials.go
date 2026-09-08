package agentgrpc

import (
	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	agentv1 "github.com/sagehou/restfleet/api/proto/gen/go/restfleet/agent/v1"
	"github.com/sagehou/restfleet/internal/domain"
)

func (s *Service) sendAgentCredential(stream agentv1.AgentControlService_ConnectServer, agentID uuid.UUID, protocol string, sequence *uint64, force bool) error {
	err := s.control.WithAgentCredential(stream.Context(), agentID, force, func(c domain.RepositoryCredential) error {
		return sendServerMessage(stream, sequence, protocol, &agentv1.ServerToAgent{Payload: &agentv1.ServerToAgent_CredentialRevision{
			CredentialRevision: &agentv1.CredentialRevision{DeliveryId: c.DeliveryID.String(), Revision: c.Revision, AgentId: c.AgentID.String(), HostId: c.HostID.String(),
				RepositoryId: c.RepositoryID.String(), GatewayId: c.GatewayID.String(), GatewayRevision: c.GatewayRevision, ResticRevision: c.ResticRevision,
				Endpoint: c.Endpoint, CaBundlePem: c.CABundlePEM, GatewayPassword: c.GatewayPassword, ResticPassword: c.ResticPassword, ValidFrom: timestamppb.New(c.ValidFrom)},
		}})
	})
	if err != nil {
		return status.Error(codes.Unavailable, "repository credential delivery unavailable")
	}
	return nil
}
