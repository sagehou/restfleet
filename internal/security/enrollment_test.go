package security

import (
	"bytes"
	"testing"
)

func TestEnrollmentTokenUsesKeyedHashAndLimitedFingerprint(t *testing.T) {
	token, err := NewEnrollmentToken()
	if err != nil {
		t.Fatal(err)
	}
	if !ValidEnrollmentToken(token) {
		t.Fatal("generated token is invalid")
	}
	first := HashEnrollmentToken(bytes.Repeat([]byte{1}, 32), token)
	second := HashEnrollmentToken(bytes.Repeat([]byte{2}, 32), token)
	if bytes.Equal(first, second) || !EnrollmentTokenHashMatches(first, bytes.Repeat([]byte{1}, 32), token) {
		t.Fatal("enrollment token hash is not correctly keyed")
	}
	fingerprint := EnrollmentTokenFingerprint(token)
	if len([]rune(fingerprint)) != 5 || fingerprint == token {
		t.Fatalf("unsafe fingerprint %q", fingerprint)
	}
}

func TestEnvelopeRoundTripAndAuthentication(t *testing.T) {
	key := bytes.Repeat([]byte{7}, 32)
	envelope, err := SealEnvelope(key, []byte("private material"), []byte("agent-ca"))
	if err != nil {
		t.Fatal(err)
	}
	plaintext, err := OpenEnvelope(key, envelope)
	if err != nil || string(plaintext) != "private material" {
		t.Fatalf("round trip failed: %q, %v", plaintext, err)
	}
	envelope.Ciphertext[0] ^= 1
	if _, err := OpenEnvelope(key, envelope); err == nil {
		t.Fatal("tampered ciphertext was accepted")
	}
}
