package security

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"testing"
)

func TestGatewayAuthorityProofAndExactReceiptDomainSeparation(t *testing.T) {
	s, central, centerKey := gatewayStatementFixture(t)
	source, sourceKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(sourceKey)
	defer clear(centerKey)
	c, err := NewGatewayAuthorityChallenge(s.Binding)
	if err != nil {
		t.Fatal(err)
	}
	proof, err := SignGatewayAuthorityChallenge(c, sourceKey)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := VerifyGatewayAuthorityChallenge(proof, source, s.Binding); err != nil || got != c {
		t.Fatal("source proof changed")
	}
	other, err := NewGatewayAuthorityChallenge(s.Binding)
	if err != nil || other.Nonce == c.Nonce {
		t.Fatal("challenge nonce reused")
	}
	hash := GatewayPendingHash([]byte("signed-statement"))
	ack, err := SignGatewayAuthorityReceipt(GatewayAuthorityReceipt{c, hash}, sourceKey)
	if err != nil || VerifyGatewayAuthorityReceipt(ack, c, hash, source) != nil {
		t.Fatal("exact receipt rejected")
	}
	for _, verify := range []func() error{
		func() error { return VerifyGatewayAuthorityReceipt(ack, other, hash, source) },
		func() error {
			return VerifyGatewayAuthorityReceipt(ack, c, GatewayPendingHash([]byte("other-statement")), source)
		},
		func() error { return VerifyGatewayAuthorityReceipt(ack, c, hash, central) },
		func() error { return VerifyGatewayAuthorityReceipt(proof, c, hash, source) },
	} {
		if verify() != ErrGatewayAuthority {
			t.Fatal("wrong receipt challenge/hash/pin/domain accepted")
		}
	}
	if _, err := VerifyGatewayAuthorityChallenge(proof, central, s.Binding); err != ErrGatewayAuthority {
		t.Fatal("wrong source pin accepted")
	}
	if _, err := VerifyGatewayMaterialChallenge(proof, source, s.Binding); err != ErrGatewayMaterial {
		t.Fatal("authority proof accepted as material initialization")
	}
	payload := proof[:len(proof)-ed25519.SignatureSize]
	for _, raw := range [][]byte{
		append([]byte(" "), payload...),
		bytes.Replace(payload, []byte(`"nonce":`), []byte(`"Nonce":`), 1),
		bytes.Replace(payload, []byte(`"binding":`), []byte(`"nonce":null,"binding":`), 1),
	} {
		bad := append(bytes.Clone(raw), ed25519.Sign(sourceKey, append([]byte(authorityChallengeContext), raw...))...)
		if _, err := VerifyGatewayAuthorityChallenge(bad, source, s.Binding); err != ErrGatewayAuthority {
			t.Fatal("signed alternative/duplicate encoding accepted")
		}
	}
	for _, wire := range [][]byte{nil, make([]byte, 64), make([]byte, 2049)} {
		if _, err := VerifyGatewayAuthorityChallenge(wire, source, s.Binding); err != ErrGatewayAuthority || VerifyGatewayAuthorityReceipt(wire, c, hash, source) != ErrGatewayAuthority {
			t.Fatal("invalid proof/receipt size accepted")
		}
	}
	for _, payload := range []any{
		struct {
			GatewayAuthorityChallenge
			Unknown bool `json:"unknown"`
		}{c, true},
		GatewayAuthorityChallenge{Binding: s.Binding},
	} {
		wire, err := signMaterialJSON(payload, sourceKey, authorityChallengeContext, 2048)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := VerifyGatewayAuthorityChallenge(wire, source, s.Binding); err != ErrGatewayAuthority {
			t.Fatal("noncanonical or empty-nonce proof accepted")
		}
	}
	proof[len(proof)-1] ^= 1
	if _, err := VerifyGatewayAuthorityChallenge(proof, source, s.Binding); err != ErrGatewayAuthority {
		t.Fatal("forged source proof accepted")
	}
}
