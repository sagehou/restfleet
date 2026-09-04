package agentgrpc

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"

	agentv1 "github.com/sagehou/restfleet/api/proto/gen/go/restfleet/agent/v1"
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
