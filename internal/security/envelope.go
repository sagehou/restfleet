package security

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
)

const EnvelopeAlgorithm = "AES-256-GCM+AES-256-GCM"

type Envelope struct {
	Ciphertext     []byte
	Nonce          []byte
	WrappedDataKey []byte
	WrapNonce      []byte
	AAD            []byte
}

func SealEnvelope(masterKey, plaintext, aad []byte) (Envelope, error) {
	if len(masterKey) != 32 {
		return Envelope{}, errors.New("master key must be 32 bytes")
	}
	dataKey := make([]byte, 32)
	if _, err := rand.Read(dataKey); err != nil {
		return Envelope{}, err
	}
	defer clear(dataKey)

	dataGCM, err := newGCM(dataKey)
	if err != nil {
		return Envelope{}, err
	}
	nonce := make([]byte, dataGCM.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return Envelope{}, err
	}
	ciphertext := dataGCM.Seal(nil, nonce, plaintext, aad)

	wrapGCM, err := newGCM(masterKey)
	if err != nil {
		return Envelope{}, err
	}
	wrapNonce := make([]byte, wrapGCM.NonceSize())
	if _, err := rand.Read(wrapNonce); err != nil {
		return Envelope{}, err
	}
	wrapped := wrapGCM.Seal(nil, wrapNonce, dataKey, wrapAAD(aad))
	return Envelope{
		Ciphertext: ciphertext, Nonce: nonce, WrappedDataKey: wrapped,
		WrapNonce: wrapNonce, AAD: append([]byte(nil), aad...),
	}, nil
}

func OpenEnvelope(masterKey []byte, envelope Envelope) ([]byte, error) {
	if len(masterKey) != 32 {
		return nil, errors.New("master key must be 32 bytes")
	}
	wrapGCM, err := newGCM(masterKey)
	if err != nil {
		return nil, err
	}
	dataKey, err := wrapGCM.Open(nil, envelope.WrapNonce, envelope.WrappedDataKey, wrapAAD(envelope.AAD))
	if err != nil {
		return nil, errors.New("unwrap data key")
	}
	defer clear(dataKey)
	dataGCM, err := newGCM(dataKey)
	if err != nil {
		return nil, err
	}
	plaintext, err := dataGCM.Open(nil, envelope.Nonce, envelope.Ciphertext, envelope.AAD)
	if err != nil {
		return nil, errors.New("decrypt secret")
	}
	return plaintext, nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("create AES cipher: %w", err)
	}
	return cipher.NewGCM(block)
}

func wrapAAD(aad []byte) []byte {
	result := make([]byte, 0, len(aad)+5)
	result = append(result, "wrap:"...)
	return append(result, aad...)
}
