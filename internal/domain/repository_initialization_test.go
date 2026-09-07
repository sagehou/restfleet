package domain

import (
	"strings"
	"testing"
)

func TestInitializationResultVocabulary(t *testing.T) {
	if !ValidInitializedRepository(strings.Repeat("a", 64), 2) {
		t.Fatal("valid result rejected")
	}
	for _, id := range []string{"", strings.Repeat("a", 63), strings.Repeat("A", 64), strings.Repeat("g", 64)} {
		if ValidInitializedRepository(id, 2) {
			t.Fatal("invalid native ID accepted")
		}
	}
	if ValidInitializedRepository(strings.Repeat("a", 64), 1) || ValidInitializedRepository(strings.Repeat("a", 64), 3) {
		t.Fatal("unknown format accepted")
	}
	for _, code := range []string{"", "INITIALIZE_FAILED", "INITIALIZE_TIMED_OUT", "REPOSITORY_LOCKED", "REPOSITORY_MISMATCH", "REPOSITORY_NOT_EMPTY", "PASSWORD_REJECTED", "REPOSITORY_UNAVAILABLE", "CONFIG_UNSAFE", "REFRESH_FAILED", "CREDENTIAL_CHANGED", "CREDENTIAL_DISABLED", "SECRET_UNAVAILABLE", "WORKER_LOST"} {
		if !ValidInitializeCode(code) {
			t.Fatalf("missing code %s", code)
		}
	}
	if ValidInitializeCode("raw-provider-secret") || ValidInitializeCode("TEST_TIMED_OUT") {
		t.Fatal("unknown error accepted")
	}
}
