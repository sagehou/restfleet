package security

import (
	"regexp"
	"strings"
)

type redactionRule struct {
	pattern     *regexp.Regexp
	replacement string
}

var redactionRules = []redactionRule{
	{regexp.MustCompile(`(?i)\b(bearer|basic)\s+[A-Za-z0-9._~+/-]+=*`), "[REDACTED]"},
	{regexp.MustCompile(`(?i)(authorization|cookie|x-csrf-token|x-restfleet-bootstrap-token)\s*[:=]\s*[^\s,;]+`), "$1: [REDACTED]"},
	{regexp.MustCompile(`(?i)(https?://)[^/@\s]+@`), "$1[REDACTED]@"},
	{regexp.MustCompile(`\brfe_[A-Za-z0-9_-]+\b`), "[REDACTED]"},
	{regexp.MustCompile(`(?is)-----BEGIN [^-]*PRIVATE KEY-----.*?-----END [^-]*PRIVATE KEY-----`), "[REDACTED]"},
	{regexp.MustCompile(`(?i)("(?:access_token|refresh_token|client_secret|password)"\s*:\s*")[^"]*(")`), "$1[REDACTED]$2"},
}

// Redact removes known exact secrets and common credential forms from diagnostic text.
func Redact(value string, knownSecrets ...string) string {
	result := value
	for _, secret := range knownSecrets {
		if secret != "" {
			result = strings.ReplaceAll(result, secret, "[REDACTED]")
		}
	}
	for _, rule := range redactionRules {
		result = rule.pattern.ReplaceAllString(result, rule.replacement)
	}
	return result
}
