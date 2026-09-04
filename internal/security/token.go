package security

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
)

// NewOpaqueToken returns a 256-bit URL-safe bearer token.
func NewOpaqueToken() (string, error) {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

// HashSecret returns the only representation persisted for opaque secrets.
func HashSecret(value string) []byte {
	sum := sha256.Sum256([]byte(value))
	return sum[:]
}

func SecretHashMatches(hash []byte, value string) bool {
	actual := HashSecret(value)
	return len(hash) == len(actual) && subtle.ConstantTimeCompare(hash, actual) == 1
}
