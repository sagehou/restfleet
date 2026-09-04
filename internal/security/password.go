package security

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Argon2Params are persisted in each encoded password hash for future upgrades.
type Argon2Params struct {
	Memory      uint32
	Iterations  uint32
	Parallelism uint8
	SaltLength  uint32
	KeyLength   uint32
}

const (
	maxArgon2Memory      = 256 * 1024
	maxArgon2Iterations  = 10
	maxArgon2Parallelism = 16
	maxArgon2SaltLength  = 64
	maxArgon2KeyLength   = 64
)

// DefaultArgon2Params follows the high-memory Argon2id profile used by M1.
var DefaultArgon2Params = Argon2Params{
	Memory:      64 * 1024,
	Iterations:  3,
	Parallelism: 2,
	SaltLength:  16,
	KeyLength:   32,
}

func validArgon2Params(params Argon2Params) bool {
	return params.Memory >= 8*uint32(params.Parallelism) && params.Memory <= maxArgon2Memory &&
		params.Iterations > 0 && params.Iterations <= maxArgon2Iterations &&
		params.Parallelism > 0 && params.Parallelism <= maxArgon2Parallelism &&
		params.SaltLength >= 8 && params.SaltLength <= maxArgon2SaltLength &&
		params.KeyLength >= 16 && params.KeyLength <= maxArgon2KeyLength
}

func HashPassword(password string, params Argon2Params) (string, error) {
	if password == "" {
		return "", errors.New("password must not be empty")
	}
	if !validArgon2Params(params) {
		return "", errors.New("invalid Argon2id parameters")
	}
	salt := make([]byte, params.SaltLength)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	hash := argon2.IDKey([]byte(password), salt, params.Iterations, params.Memory, params.Parallelism, params.KeyLength)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version,
		params.Memory,
		params.Iterations,
		params.Parallelism,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(hash),
	), nil
}

func VerifyPassword(encoded, password string) (bool, error) {
	if len(encoded) > 512 {
		return false, errors.New("invalid password hash")
	}
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false, errors.New("invalid password hash")
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version ||
		parts[2] != fmt.Sprintf("v=%d", version) {
		return false, errors.New("unsupported password hash version")
	}
	var params Argon2Params
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &params.Memory, &params.Iterations, &params.Parallelism); err != nil ||
		parts[3] != fmt.Sprintf("m=%d,t=%d,p=%d", params.Memory, params.Iterations, params.Parallelism) {
		return false, errors.New("invalid password hash parameters")
	}
	salt, err := base64.RawStdEncoding.Strict().DecodeString(parts[4])
	if err != nil {
		return false, errors.New("invalid password hash salt")
	}
	expected, err := base64.RawStdEncoding.Strict().DecodeString(parts[5])
	if err != nil || len(expected) < 16 {
		return false, errors.New("invalid password hash value")
	}
	params.SaltLength = uint32(len(salt))
	params.KeyLength = uint32(len(expected))
	if !validArgon2Params(params) {
		return false, errors.New("invalid password hash parameters")
	}
	actual := argon2.IDKey([]byte(password), salt, params.Iterations, params.Memory, params.Parallelism, params.KeyLength)
	return subtle.ConstantTimeCompare(actual, expected) == 1, nil
}
