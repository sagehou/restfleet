package security

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"strings"
)

const enrollmentTokenPrefix = "rfe_"

func NewEnrollmentToken() (string, error) {
	value, err := NewOpaqueToken()
	if err != nil {
		return "", err
	}
	return enrollmentTokenPrefix + value, nil
}

func ValidEnrollmentToken(value string) bool {
	if !strings.HasPrefix(value, enrollmentTokenPrefix) {
		return false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(value, enrollmentTokenPrefix))
	return err == nil && len(decoded) == 32
}

func HashEnrollmentToken(pepper []byte, value string) []byte {
	hash := hmac.New(sha256.New, pepper)
	_, _ = hash.Write([]byte(value))
	return hash.Sum(nil)
}

func EnrollmentTokenHashMatches(hash, pepper []byte, value string) bool {
	actual := HashEnrollmentToken(pepper, value)
	return len(hash) == len(actual) && subtle.ConstantTimeCompare(hash, actual) == 1
}

// EnrollmentTokenFingerprint exposes only the last four token characters.
func EnrollmentTokenFingerprint(value string) string {
	if len(value) <= 4 {
		return "…"
	}
	return "…" + value[len(value)-4:]
}

// DeriveEnrollmentPepper domain-separates token hashes from the master key.
func DeriveEnrollmentPepper(masterKey []byte) []byte {
	hash := hmac.New(sha256.New, masterKey)
	_, _ = hash.Write([]byte("restfleet/enrollment-token/v1"))
	return hash.Sum(nil)
}
