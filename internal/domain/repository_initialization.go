package domain

import "regexp"

var nativeRepositoryID = regexp.MustCompile(`^[0-9a-f]{64}$`)

func ValidInitializedRepository(id string, version int) bool {
	return version == 2 && nativeRepositoryID.MatchString(id)
}

func ValidInitializeCode(code string) bool {
	switch code {
	case "", "INITIALIZE_FAILED", "INITIALIZE_TIMED_OUT", "REPOSITORY_LOCKED", "REPOSITORY_MISMATCH",
		"REPOSITORY_NOT_EMPTY", "PASSWORD_REJECTED", "REPOSITORY_UNAVAILABLE", "CONFIG_UNSAFE", "REFRESH_FAILED",
		"CREDENTIAL_CHANGED", "CREDENTIAL_DISABLED", "SECRET_UNAVAILABLE", "WORKER_LOST":
		return true
	}
	return false
}
