package security

import (
	"strings"
	"testing"
)

func TestRedactCanarySecrets(t *testing.T) {
	canary := "restfleet-canary-secret-9f6a"
	input := "Authorization: Bearer abc.def Cookie=session-value url=https://user:pass@example.invalid " +
		"{\"refresh_token\":\"refresh-value\",\"password\":\"password-value\"} " + canary
	output := Redact(input, canary)
	for _, forbidden := range []string{"abc.def", "session-value", "user:pass", "refresh-value", "password-value", canary} {
		if strings.Contains(output, forbidden) {
			t.Fatalf("redaction leaked %q in %q", forbidden, output)
		}
	}
}
