package security

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/sagehou/restfleet/internal/domain"
	"golang.org/x/crypto/nacl/box"
)

func pendingFixture(t *testing.T) (GatewayPendingRecord, ed25519.PublicKey, ed25519.PrivateKey, [32]byte, []byte) {
	t.Helper()
	s, public, private := gatewayStatementFixture(t)
	recipient, recipientPrivate, err := box.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	r := GatewayPendingRecord{Header: GatewayPendingHeader{Binding: s.Binding, RecordID: uuid.Must(uuid.NewV7()), Sequence: 1, PreviousHash: strings.Repeat("0", 64), AuthorizationRevision: 1, CreatedAt: s.IssuedAt}, Kind: "refresh", ExpectedSecretRevision: 1, Config: []byte("config-secret-canary")}
	return r, public, private, *recipient, recipientPrivate[:]
}

func TestGatewayPendingIndependentAuthenticationAndEncryption(t *testing.T) {
	r, public, source, recipient, private := pendingFixture(t)
	wire, err := SealGatewayPending(r, recipient, source)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(wire, r.Config) {
		t.Fatal("plaintext leaked")
	}
	got, err := OpenGatewayPending(wire, public, private)
	if err != nil || got.Header != r.Header || !bytes.Equal(got.Config, r.Config) {
		t.Fatal("round trip failed")
	}
	for _, position := range []int{0, 10, len(wire) / 2, len(wire) - 65, len(wire) - 1} {
		bad := append([]byte(nil), wire...)
		bad[position] ^= 1
		if _, err := OpenGatewayPending(bad, public, private); err != ErrGatewayPending {
			t.Fatal("tamper accepted")
		}
	}
	wrongPublic, wrongSource, _ := ed25519.GenerateKey(rand.Reader)
	for _, key := range []ed25519.PublicKey{wrongPublic, nil, public[:31]} {
		if _, err = OpenGatewayPending(wire, key, private); err != ErrGatewayPending {
			t.Fatal("unregistered source accepted")
		}
	}
	// Possession of the recipient PUBLIC key does not establish origin identity.
	forged, err := SealGatewayPending(r, recipient, wrongSource)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = OpenGatewayPending(forged, public, private); err != ErrGatewayPending {
		t.Fatal("encryption confused with authentication")
	}
	_, wrongPrivate, _ := box.GenerateKey(rand.Reader)
	if _, err = OpenGatewayPending(wire, public, wrongPrivate[:]); err != ErrGatewayPending {
		t.Fatal("wrong recipient accepted")
	}
	for _, bad := range [][]byte{nil, wire[:63], append(wire, 0), make([]byte, MaxGatewayPendingSize+1)} {
		if _, err = InspectGatewayPending(bad, public); err != ErrGatewayPending {
			t.Fatal("invalid length accepted")
		}
	}
}

func TestGatewayPendingStrictCanonicalAndInnerBinding(t *testing.T) {
	r, public, source, recipient, private := pendingFixture(t)
	wire, err := SealGatewayPending(r, recipient, source)
	if err != nil {
		t.Fatal(err)
	}
	payload := wire[:len(wire)-64]
	for _, raw := range [][]byte{append([]byte(" "), payload...), append(append([]byte(nil), payload...), '\n'), bytes.Replace(payload, []byte(`"sequence":1`), []byte(`"sequence":1,"sequence":1`), 1), bytes.Replace(payload, []byte(`"sequence":1`), []byte(`"sequence":1,"unknown":true`), 1)} {
		bad := append(raw, ed25519.Sign(source, append([]byte(pendingContext), raw...))...)
		if _, err = InspectGatewayPending(bad, public); err != ErrGatewayPending {
			t.Fatal("ambiguous signed encoding accepted")
		}
	}
	var outer gatewayPendingWire
	if json.Unmarshal(payload, &outer) != nil {
		t.Fatal("fixture decode")
	}
	outer.Header.Binding.HostID = uuid.Must(uuid.NewV7())
	changed, _ := json.Marshal(outer)
	transplant := append(changed, ed25519.Sign(source, append([]byte(pendingContext), changed...))...)
	if _, err = OpenGatewayPending(transplant, public, private); err != ErrGatewayPending {
		t.Fatal("ciphertext transplanted across binding")
	}
	// Inner JSON is strict as well, even with a valid outer source signature.
	plain, _ := json.Marshal(r)
	plain = bytes.Replace(plain, []byte(`"kind":"refresh"`), []byte(`"kind":"refresh","unknown":true`), 1)
	outer.Header = r.Header
	outer.Ciphertext, err = box.SealAnonymous(nil, plain, &recipient, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	changed, _ = json.Marshal(outer)
	bad := append(changed, ed25519.Sign(source, append([]byte(pendingContext), changed...))...)
	if _, err = OpenGatewayPending(bad, public, private); err != ErrGatewayPending {
		t.Fatal("noncanonical inner record accepted")
	}
}

func TestGatewayPendingAuditAndLimits(t *testing.T) {
	r, _, source, recipient, _ := pendingFixture(t)
	r.Config = bytes.Repeat([]byte{1}, 256<<10)
	if wire, err := SealGatewayPending(r, recipient, source); err != nil || len(wire) > MaxGatewayPendingSize {
		t.Fatal("maximum config cannot be queued")
	}
	r.Config = append(r.Config, 1)
	if _, err := SealGatewayPending(r, recipient, source); err != ErrGatewayPending {
		t.Fatal("oversized config accepted")
	}
	r.Config = nil
	r.ExpectedSecretRevision = 0
	r.Kind = "audit"
	r.Event = &domain.GatewayEvent{Binding: domain.GatewayBinding{HostID: r.Header.Binding.HostID, RepositoryID: r.Header.Binding.RepositoryID, GatewayID: r.Header.Binding.GatewayID, OperationID: uuid.Must(uuid.NewV7())}, Action: "session_start", Reason: "requested"}
	if _, err := SealGatewayPending(r, recipient, source); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*GatewayPendingRecord){
		func(r *GatewayPendingRecord) { r.Event.Binding.HostID = uuid.Must(uuid.NewV7()) },
		func(r *GatewayPendingRecord) { r.Event.Reason = "secret-canary" },
		func(r *GatewayPendingRecord) { r.Config = []byte("secret-canary") },
		func(r *GatewayPendingRecord) { r.Config = []byte{} },
		func(r *GatewayPendingRecord) { r.Header.Sequence = 0 },
		func(r *GatewayPendingRecord) { r.Header.PreviousHash = strings.Repeat("a", 64) },
		func(r *GatewayPendingRecord) { r.Header.AuthorizationRevision = 0 },
	} {
		bad := r
		event := *r.Event
		bad.Event = &event
		mutate(&bad)
		if _, err := SealGatewayPending(bad, recipient, source); err != ErrGatewayPending || strings.Contains(err.Error(), "canary") {
			t.Fatal("invalid input accepted or echoed")
		}
	}
}

func TestGatewayPendingReceiptExactCommitAndDomainSeparation(t *testing.T) {
	r, public, key, recipient, _ := pendingFixture(t)
	wire, err := SealGatewayPending(r, recipient, key)
	if err != nil {
		t.Fatal(err)
	}
	ack := GatewayPendingReceipt{AdmissionID: r.Header.Binding.AdmissionID, RuntimeID: r.Header.Binding.RuntimeID, Sequence: 1, RecordID: r.Header.RecordID, WireHash: GatewayPendingHash(wire)}
	signed, err := SignGatewayPendingReceipt(ack, key)
	if err != nil {
		t.Fatal(err)
	}
	got, err := VerifyGatewayPendingReceipt(signed, public)
	if err != nil || got != ack {
		t.Fatal("receipt round trip")
	}
	if _, err = InspectGatewayPending(signed, public); err != ErrGatewayPending {
		t.Fatal("ACK authorizes a record")
	}
	if _, err = VerifyGatewayStatement(signed, public); err != ErrGatewayStatement {
		t.Fatal("ACK authorizes a grant")
	}
	for _, bad := range [][]byte{wire, signed[:63], append(signed, 0)} {
		if _, err = VerifyGatewayPendingReceipt(bad, public); err != ErrGatewayPending {
			t.Fatal("bad ACK accepted")
		}
	}
}
