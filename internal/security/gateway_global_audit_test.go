package security

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/sagehou/restfleet/internal/domain"
)

func globalAuditFixture(t *testing.T) (GatewayPendingRecord, [32]byte, ed25519.PublicKey, ed25519.PrivateKey, []byte) {
	t.Helper()
	r, public, source, recipient, private := pendingFixture(t)
	r.Header.Binding = GatewayAuthorizationBinding{}
	r.Header.AuthorizationRevision = 0
	r.Header.AuditOrigin = GatewayAuditBinding{OriginID: uuid.Must(uuid.NewV7()), RuntimeID: uuid.Must(uuid.NewV7())}
	r.Kind, r.Config, r.ExpectedSecretRevision = "global_audit", nil, 0
	r.Event = &domain.GatewayEvent{Action: "denied", Reason: "route_unavailable"}
	return r, recipient, public, source, private
}

func TestGlobalAuditCodecCannotSupplyAuthorityOrRepositoryIdentity(t *testing.T) {
	r, recipient, public, source, private := globalAuditFixture(t)
	wire, err := SealGatewayPending(r, recipient, source)
	if err != nil {
		t.Fatal(err)
	}
	got, err := OpenGatewayPending(wire, public, private)
	if err != nil || got.Header != r.Header || got.Event.Binding != (domain.GatewayBinding{}) || got.Config != nil {
		t.Fatal("global audit round trip")
	}
	if b, err := GatewayPendingAuditIdentity(wire); err != nil || b != r.Header.AuditOrigin {
		t.Fatal("global selector changed")
	}
	if _, _, err := GatewayPendingIdentity(wire); err != ErrGatewayPending {
		t.Fatal("global record selected admission registry")
	}
	if _, err := VerifyGatewayStatement(wire, public); err != ErrGatewayStatement {
		t.Fatal("audit granted access")
	}
	for _, mutate := range []func(*GatewayPendingRecord){
		func(r *GatewayPendingRecord) {
			r.Kind = "refresh"
			r.Event = nil
			r.Config = []byte("private-config-canary")
			r.ExpectedSecretRevision = 1
		},
		func(r *GatewayPendingRecord) { r.Kind = "audit" },
		func(r *GatewayPendingRecord) { r.Header.AuthorizationRevision = 1 },
		func(r *GatewayPendingRecord) { r.Header.Binding.HostID = uuid.Must(uuid.NewV7()) },
		func(r *GatewayPendingRecord) { r.Header.AuditOrigin.RuntimeID = uuid.Nil },
		func(r *GatewayPendingRecord) {
			r.Event = &domain.GatewayEvent{Action: "session_start", Reason: "requested"}
		},
		func(r *GatewayPendingRecord) {
			r.Event = &domain.GatewayEvent{Action: "denied", Reason: "route_unavailable", Authenticated: true}
		},
		func(r *GatewayPendingRecord) { r.Config = []byte{} },
	} {
		bad := r
		mutate(&bad)
		if _, err := SealGatewayPending(bad, recipient, source); err != ErrGatewayPending {
			t.Fatal("audit mode accepted secret/scoped/authorized data")
		}
	}
	payload := wire[:len(wire)-64]
	wrongDomain := append(bytes.Clone(payload), ed25519.Sign(source, append([]byte(pendingContext), payload...))...)
	if _, err := InspectGatewayPending(wrongDomain, public); err != ErrGatewayPending {
		t.Fatal("repository signature domain accepted")
	}
	for _, raw := range [][]byte{
		append([]byte(" "), payload...),
		bytes.Replace(payload, []byte(`"audit_origin":`), []byte(`"unknown":true,"audit_origin":`), 1),
		bytes.Replace(payload, []byte(`"audit_origin":`), []byte(`"Audit_origin":`), 1),
	} {
		bad := append(bytes.Clone(raw), ed25519.Sign(source, append([]byte(globalPendingContext), raw...))...)
		if _, err := InspectGatewayPending(bad, public); err != ErrGatewayPending {
			t.Fatal("noncanonical global wire accepted")
		}
	}
	ack := GatewayPendingReceipt{AuditOriginID: r.Header.AuditOrigin.OriginID, RuntimeID: r.Header.AuditOrigin.RuntimeID,
		Sequence: 1, RecordID: r.Header.RecordID, WireHash: GatewayPendingHash(wire)}
	signed, err := SignGatewayPendingReceipt(ack, source)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := VerifyGatewayPendingReceipt(signed, public); err != nil || got != ack {
		t.Fatal("global receipt round trip")
	}
	ack.AdmissionID = uuid.Must(uuid.NewV7())
	if _, err := SignGatewayPendingReceipt(ack, source); err != ErrGatewayPending {
		t.Fatal("ambiguous admission/audit receipt accepted")
	}
	ack.AdmissionID = uuid.Nil
	raw, _ := json.Marshal(ack)
	wrongDomain = append(raw, ed25519.Sign(source, append([]byte(receiptContext), raw...))...)
	if _, err := VerifyGatewayPendingReceipt(wrongDomain, public); err != ErrGatewayPending {
		t.Fatal("repository receipt domain accepted")
	}
}

func TestRepositoryPendingEncodingRemainsUnchanged(t *testing.T) {
	r, public, source, recipient, _ := pendingFixture(t)
	wire, err := SealGatewayPending(r, recipient, source)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(wire[:len(wire)-64], []byte("audit_origin")) {
		t.Fatal("legacy payload changed")
	}
	if _, err := GatewayPendingAuditIdentity(wire); err != ErrGatewayPending {
		t.Fatal("repository record selected global registry")
	}
	ack, err := SignGatewayPendingReceipt(GatewayPendingReceipt{AdmissionID: r.Header.Binding.AdmissionID, RuntimeID: r.Header.Binding.RuntimeID,
		Sequence: 1, RecordID: r.Header.RecordID, WireHash: strings.Repeat("a", 64)}, source)
	if err != nil || bytes.Contains(ack[:len(ack)-64], []byte("audit_origin")) {
		t.Fatal("legacy receipt changed")
	}
	if _, err := VerifyGatewayPendingReceipt(ack, public); err != nil {
		t.Fatal(err)
	}
}
