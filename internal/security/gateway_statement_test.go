package security

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"math"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func gatewayStatementFixture(t *testing.T) (GatewayStatement, ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.MustParse("019abcde-1234-7000-8000-000000000001")
	s := GatewayStatement{Binding: GatewayAuthorizationBinding{
		AdmissionID: id, Owner: id, RuntimeID: id, AgentID: id, HostID: id, RepositoryID: id,
		GatewayID: id, StorageCredentialID: id, DeliveryID: id, GatewaySecretRef: id, ResticSecretRef: id,
		ConfigurationHash: strings.Repeat("a", 64),
	}, Revision: 1, IssuedAt: 1800000000, ExpiresAt: 1800000000 + MaxGatewayAuthorizationSeconds}
	return s, public, private
}

func TestGatewayStatementRoundTrip(t *testing.T) {
	s, public, private := gatewayStatementFixture(t)
	for _, revoked := range []bool{false, true} {
		s.Revoked = revoked
		if revoked {
			s.ExpiresAt = 0
		}
		wire, err := SignGatewayStatement(s, private)
		if err != nil {
			t.Fatal(err)
		}
		got, err := VerifyGatewayStatement(wire, public)
		if err != nil || got != s {
			t.Fatalf("round trip: %v", err)
		}
		// Every byte, including the signature, is authenticated.
		for i := range wire {
			wire[i] ^= 1
			got, err = VerifyGatewayStatement(wire, public)
			if err != ErrGatewayStatement || got != (GatewayStatement{}) {
				t.Fatalf("tamper accepted at %d", i)
			}
			wire[i] ^= 1
		}
	}
}

func TestGatewayStatementRejectsInvalidClaims(t *testing.T) {
	s, public, private := gatewayStatementFixture(t)
	changes := []func(*GatewayStatement){
		func(s *GatewayStatement) { s.Revision = 0 },
		func(s *GatewayStatement) { s.Revision = math.MaxUint64 },
		func(s *GatewayStatement) { s.IssuedAt = 0 },
		func(s *GatewayStatement) { s.IssuedAt = math.MinInt64 },
		func(s *GatewayStatement) { s.ExpiresAt = math.MaxInt64 },
		func(s *GatewayStatement) { s.ExpiresAt++ },
		func(s *GatewayStatement) { s.ExpiresAt = s.IssuedAt },
		func(s *GatewayStatement) { s.Revoked = true },
		func(s *GatewayStatement) { s.Binding.ConfigurationHash = strings.Repeat("A", 64) },
		func(s *GatewayStatement) { s.Binding.ConfigurationHash = "secret-canary" },
	}
	for i := range 11 {
		changes = append(changes, func(s *GatewayStatement) {
			b := &s.Binding
			ids := []*uuid.UUID{&b.AdmissionID, &b.Owner, &b.RuntimeID, &b.AgentID, &b.HostID, &b.RepositoryID,
				&b.GatewayID, &b.StorageCredentialID, &b.DeliveryID, &b.GatewaySecretRef, &b.ResticSecretRef}
			*ids[i] = uuid.New() // UUIDv4 must fail too, not only nil.
		})
	}
	for i, change := range changes {
		bad := s
		change(&bad)
		if wire, err := SignGatewayStatement(bad, private); err != ErrGatewayStatement || wire != nil {
			t.Fatalf("invalid claim signed: %d", i)
		}
		payload, err := json.Marshal(bad)
		if err != nil {
			t.Fatal(err)
		}
		wire := append(payload, ed25519.Sign(private, append([]byte(gatewayStatementContext), payload...))...)
		if got, err := VerifyGatewayStatement(wire, public); err != ErrGatewayStatement || got != (GatewayStatement{}) {
			t.Fatalf("signed invalid claim accepted: %d", i)
		}
	}
}

func TestGatewayStatementStrictWireAndKeys(t *testing.T) {
	s, public, private := gatewayStatementFixture(t)
	payload, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{
		" " + string(payload), string(payload) + "\n", string(payload) + "{}",
		strings.Replace(string(payload), `"revision":1`, `"revision":1,"revision":1`, 1),
		strings.Replace(string(payload), `"revision":1`, `"revision":1,"unknown":true`, 1),
		strings.Replace(string(payload), `"revoked":false`, `"revoked":null`, 1),
		strings.Replace(string(payload), `"revision":1`, `"revision":1e0`, 1),
		strings.Repeat("x", MaxGatewayStatementSize),
	} {
		wire := append([]byte(raw), ed25519.Sign(private, append([]byte(gatewayStatementContext), raw...))...)
		if _, err := VerifyGatewayStatement(wire, public); err != ErrGatewayStatement {
			t.Fatal("noncanonical wire accepted")
		}
	}
	wire, err := SignGatewayStatement(s, private)
	if err != nil {
		t.Fatal(err)
	}
	for n := 0; n < len(wire); n++ {
		if _, err := VerifyGatewayStatement(wire[:n], public); err != ErrGatewayStatement {
			t.Fatal("truncation accepted")
		}
	}
	for _, key := range []ed25519.PublicKey{nil, public[:31], make([]byte, 33), make([]byte, 32)} {
		if _, err := VerifyGatewayStatement(wire, key); err != ErrGatewayStatement {
			t.Fatal("invalid public key accepted")
		}
	}
	for _, key := range []ed25519.PrivateKey{nil, private[:63], make([]byte, 65)} {
		if _, err := SignGatewayStatement(s, key); err != ErrGatewayStatement {
			t.Fatal("invalid private key accepted")
		}
	}
	// A signature from another protocol must not authorize this one.
	wrong := append(payload, ed25519.Sign(private, payload)...)
	if _, err := VerifyGatewayStatement(wrong, public); err != ErrGatewayStatement {
		t.Fatal("signature context ignored")
	}
}

func FuzzGatewayStatement(f *testing.F) {
	key := make(ed25519.PublicKey, ed25519.PublicKeySize)
	f.Add([]byte("invalid"))
	f.Add(make([]byte, 64))
	f.Fuzz(func(t *testing.T, wire []byte) {
		got, err := VerifyGatewayStatement(wire, key)
		if err != nil && (err != ErrGatewayStatement || got != (GatewayStatement{})) {
			t.Fatal("unsafe error result")
		}
	})
}
