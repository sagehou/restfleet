package agentgrpc

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"

	agentv1 "github.com/sagehou/restfleet/api/proto/gen/go/restfleet/agent/v1"
	"github.com/sagehou/restfleet/internal/domain"
	"github.com/sagehou/restfleet/internal/security"
	control "github.com/sagehou/restfleet/internal/server"
)

func TestProtocolNegotiationSupportsNAndNMinusOne(t *testing.T) {
	if got := selectProtocol([]string{"1.0"}); got != "1.0" {
		t.Fatalf("current protocol selected %q", got)
	}
	if got := selectProtocol([]string{"0.9"}); got != "0.9" {
		t.Fatalf("previous protocol selected %q", got)
	}
	if got := selectProtocol([]string{"2.0"}); got != "" {
		t.Fatalf("incompatible protocol selected %q", got)
	}
}

func TestAgentPayloadCannotSelfAssertAgentID(t *testing.T) {
	hello := (&agentv1.Hello{}).ProtoReflect().Descriptor()
	if hello.Fields().ByName("agent_id") != nil || hello.Fields().ByName("host_id") != nil {
		t.Fatal("Hello contract permits a self-asserted authorization identity")
	}
}

func TestEnvelopeRequiresUUIDv7AndKnownProtocol(t *testing.T) {
	id, err := uuid.NewV7()
	if err != nil {
		t.Fatal(err)
	}
	if err := validateEnvelope(id.String(), "1.0", timestamppb.New(time.Now())); err != nil {
		t.Fatal(err)
	}
	if err := validateEnvelope(uuid.NewString(), "1.0", timestamppb.Now()); err == nil {
		t.Fatal("non-v7 message ID was accepted")
	}
	if err := validateEnvelope(id.String(), "2.0", timestamppb.Now()); err == nil {
		t.Fatal("unknown protocol was accepted")
	}
}

type captureConnectStream struct {
	agentv1.AgentControlService_ConnectServer
	ctx  context.Context
	sent []*agentv1.ServerToAgent
}

func (s *captureConnectStream) Context() context.Context {
	return s.ctx
}

func (s *captureConnectStream) Send(message *agentv1.ServerToAgent) error {
	s.sent = append(s.sent, message)
	return nil
}

func TestDesiredStateIsReplayedAfterServerRestart(t *testing.T) {
	agentID := uuid.Must(uuid.NewV7())
	desired, err := domain.NewDefaultDesiredState(agentID, 42, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	store := &grpcTestStore{desired: desired}
	controlPlane, err := control.NewControlPlane(store, control.Settings{
		BootstrapToken: "unused",
		PasswordParams: security.Argon2Params{
			Memory: 64, Iterations: 1, Parallelism: 1, SaltLength: 8, KeyLength: 16,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		service := New(controlPlane, nil, 15*time.Second)
		stream := &captureConnectStream{ctx: context.Background()}
		sequence := uint64(1)
		if err := service.sendDesiredState(stream, agentID, "1.0", &sequence); err != nil {
			t.Fatal(err)
		}
		if len(stream.sent) != 1 ||
			stream.sent[0].GetDesiredStateSnapshot().GetRevision() != 42 ||
			stream.sent[0].GetSequence() != 2 {
			t.Fatalf("replayed desired state = %+v", stream.sent)
		}
	}
	if store.published != 2 {
		t.Fatalf("published count = %d, want replay after restart", store.published)
	}
}
