package security

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"golang.org/x/crypto/nacl/box"
)

func materialFixture(t *testing.T) (GatewayMaterial, ed25519.PublicKey, ed25519.PrivateKey, ed25519.PrivateKey, []byte) {
	t.Helper()
	s, public, central := gatewayStatementFixture(t)
	recipient, private, _ := box.GenerateKey(rand.Reader)
	pending, _, _ := box.GenerateKey(rand.Reader)
	sourcePublic, source, _ := ed25519.GenerateKey(rand.Reader)
	challenge, err := NewGatewayMaterialChallenge(s.Binding, *recipient)
	if err != nil {
		t.Fatal(err)
	}
	statement, err := SignGatewayStatement(s, central)
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte("cloud-secret-canary")
	return GatewayMaterial{Challenge: challenge, Source: sourcePublic, PendingRecipient: *pending, Statement: statement,
		AdmissionCreatedAt: s.IssuedAt - 1, AdmissionExpiresAt: s.ExpiresAt, SecretRevision: 1, Remote: "encrypted",
		ConfigHash: GatewayPendingHash(raw), Config: raw}, public, central, source, private[:]
}

func TestGatewayMaterialProofEncryptionAndExactReceipt(t *testing.T) {
	m, public, central, source, private := materialFixture(t)
	proof, err := SignGatewayMaterialChallenge(m.Challenge, source)
	if err != nil {
		t.Fatal(err)
	}
	got, err := VerifyGatewayMaterialChallenge(proof, m.Source, m.Challenge.Binding)
	if err != nil || got != m.Challenge {
		t.Fatal("source proof round trip")
	}
	wire, err := SealGatewayMaterial(m, central)
	if err != nil || bytes.Contains(wire, m.Config) {
		t.Fatal("material not encrypted")
	}
	opened, err := OpenGatewayMaterial(wire, m.Challenge, m.Source, public, private)
	if err != nil || !bytes.Equal(opened.Config, m.Config) || opened.SecretRevision != 1 {
		t.Fatal("material round trip")
	}
	defer clear(opened.Config)
	receipt, err := SignGatewayMaterialReceipt(GatewayMaterialReceipt{Challenge: m.Challenge, WireHash: GatewayPendingHash(wire)}, source)
	if err != nil || VerifyGatewayMaterialReceipt(receipt, m.Challenge, GatewayPendingHash(wire), m.Source) != nil {
		t.Fatal("receipt")
	}
	changed := m.Challenge
	changed.Nonce[0] ^= 1
	if VerifyGatewayMaterialReceipt(receipt, changed, GatewayPendingHash(wire), m.Source) != ErrGatewayMaterial ||
		VerifyGatewayMaterialReceipt(receipt, m.Challenge, GatewayPendingHash(proof), m.Source) != ErrGatewayMaterial {
		t.Fatal("receipt accepted for another exchange")
	}
	for _, bad := range []GatewayMaterialChallenge{changed, func() GatewayMaterialChallenge {
		c := m.Challenge
		c.Binding.RuntimeID = uuid.Must(uuid.NewV7())
		return c
	}()} {
		if _, err := OpenGatewayMaterial(wire, bad, m.Source, public, private); err != ErrGatewayMaterial {
			t.Fatal("replay/foreign binding accepted")
		}
	}
	wrongPublic, wrongSource, _ := ed25519.GenerateKey(rand.Reader)
	if _, err = VerifyGatewayMaterialChallenge(proof, wrongPublic, m.Challenge.Binding); err != ErrGatewayMaterial {
		t.Fatal("unregistered source proof")
	}
	if _, err = OpenGatewayMaterial(wire, m.Challenge, wrongPublic, public, private); err != ErrGatewayMaterial {
		t.Fatal("source transplant")
	}
	forged, err := SealGatewayMaterial(m, wrongSource)
	if err != ErrGatewayMaterial || forged != nil { // statement is pinned to the real center
		t.Fatal("untrusted center sealed a trusted statement")
	}
	_, wrongPrivate, _ := box.GenerateKey(rand.Reader)
	if _, err = OpenGatewayMaterial(wire, m.Challenge, m.Source, public, wrongPrivate[:]); err != ErrGatewayMaterial {
		t.Fatal("wrong recipient decrypted")
	}
	for _, index := range []int{0, len(wire) / 2, len(wire) - 1} {
		bad := append([]byte(nil), wire...)
		bad[index] ^= 1
		if _, err = OpenGatewayMaterial(bad, m.Challenge, m.Source, public, private); err != ErrGatewayMaterial {
			t.Fatal("tamper")
		}
	}
}

func TestGatewayMaterialStrictCanonicalAndLimits(t *testing.T) {
	m, public, central, _, private := materialFixture(t)
	wire, err := SealGatewayMaterial(m, central)
	if err != nil {
		t.Fatal(err)
	}
	payload := wire[:len(wire)-64]
	for _, raw := range [][]byte{append([]byte(" "), payload...), bytes.Replace(payload, []byte(`"challenge":`), []byte(`"unknown":true,"challenge":`), 1), append(append([]byte(nil), payload...), '\n')} {
		bad := append(raw, ed25519.Sign(central, append([]byte(materialContext), raw...))...)
		if _, err = OpenGatewayMaterial(bad, m.Challenge, m.Source, public, private); err != ErrGatewayMaterial {
			t.Fatal("noncanonical outer accepted")
		}
	}
	var outer gatewayMaterialWire
	if json.Unmarshal(payload, &outer) != nil {
		t.Fatal("outer")
	}
	plain, _ := json.Marshal(m)
	plain = bytes.Replace(plain, []byte(`"secret_revision":1`), []byte(`"secret_revision":1,"secret_revision":1`), 1)
	outer.Ciphertext, _ = box.SealAnonymous(nil, plain, &m.Challenge.Recipient, rand.Reader)
	bad, _ := signMaterialJSON(outer, central, materialContext, MaxGatewayMaterialSize)
	if _, err = OpenGatewayMaterial(bad, m.Challenge, m.Source, public, private); err != ErrGatewayMaterial {
		t.Fatal("noncanonical inner accepted")
	}
	for _, change := range []func(*GatewayMaterial){
		func(m *GatewayMaterial) { m.ConfigHash = GatewayPendingHash([]byte("other")) },
		func(m *GatewayMaterial) { m.Config = make([]byte, 256<<10+1) },
		func(m *GatewayMaterial) { m.SecretRevision = 0 },
		func(m *GatewayMaterial) { m.AdmissionExpiresAt-- },
		func(m *GatewayMaterial) { m.AdmissionCreatedAt++; m.AdmissionCreatedAt++ },
		func(m *GatewayMaterial) { m.Source = nil },
		func(m *GatewayMaterial) { m.PendingRecipient = [32]byte{} },
		func(m *GatewayMaterial) { m.Challenge.Binding.HostID = uuid.Must(uuid.NewV7()) },
	} {
		bad := m
		change(&bad)
		if _, err := SealGatewayMaterial(bad, central); err != ErrGatewayMaterial {
			t.Fatal("invalid material sealed")
		}
	}
	for _, raw := range [][]byte{nil, make([]byte, 64), make([]byte, MaxGatewayMaterialSize+1)} {
		if _, err := OpenGatewayMaterial(raw, m.Challenge, m.Source, public, private); err != ErrGatewayMaterial {
			t.Fatal("length")
		}
	}
	maximum := m
	maximum.Config = bytes.Repeat([]byte("a"), 256<<10)
	maximum.ConfigHash = GatewayPendingHash(maximum.Config)
	if _, err := SealGatewayMaterial(maximum, central); err != nil {
		t.Fatal("maximum allowed configuration cannot fit wire limit")
	}
}

func TestGatewayMaterialRejectsInconsistentPrivateKeys(t *testing.T) {
	m, _, central, source, _ := materialFixture(t)
	for _, key := range []ed25519.PrivateKey{nil, source[:32], append(ed25519.PrivateKey(nil), source...)} {
		if len(key) == ed25519.PrivateKeySize {
			key[63] ^= 1
		}
		if wire, err := SignGatewayMaterialChallenge(m.Challenge, key); err != ErrGatewayMaterial || wire != nil {
			t.Fatal("invalid source private key accepted")
		}
		if wire, err := SignGatewayMaterialReceipt(GatewayMaterialReceipt{Challenge: m.Challenge, WireHash: GatewayPendingHash(m.Config)}, key); err != ErrGatewayMaterial || wire != nil {
			t.Fatal("invalid receipt private key accepted")
		}
	}
	central[63] ^= 1
	if wire, err := SealGatewayMaterial(m, central); err != ErrGatewayMaterial || wire != nil {
		t.Fatal("invalid center private key accepted")
	}
}
