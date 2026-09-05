package rclone

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"
)

func testConfig() string {
	return "[cloud]\ntype = onedrive\ntoken = {\"access_token\":\"canary-access\",\"token_type\":\"Bearer\",\"refresh_token\":\"canary-refresh\",\"expiry\":\"2030-01-01T00:00:00Z\"}\ndrive_id = example-drive\ndrive_type = personal\n[encrypted]\ntype = crypt\nremote = cloud:backups\npassword = " + base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{5}, 32)) + "\n"
}

func TestRestrictedConfig(t *testing.T) {
	valid := testConfig()
	config, err := ParseConfig(valid, "encrypted")
	if err != nil {
		t.Fatal(err)
	}
	normalized := config.Bytes()
	defer clear(normalized)
	reparsed, err := ParseConfig(string(normalized), "encrypted")
	if err != nil || !config.SameTarget(reparsed) {
		t.Fatal("canonical config did not round-trip")
	}
	replacement, err := ParseConfig(strings.ReplaceAll(valid, "canary-refresh", "new-refresh"), "encrypted")
	if err != nil || !config.SameTarget(replacement) {
		t.Fatal("token refresh changed target")
	}
	for name, raw := range map[string]string{
		"other-backend":       strings.Replace(valid, "type = onedrive", "type = sftp", 1),
		"custom-endpoint":     strings.Replace(valid, "drive_type = personal", "drive_type = personal\ntoken_url = http://169.254.169.254/token", 1),
		"custom-region":       strings.Replace(valid, "drive_type = personal", "drive_type = personal\nregion = china", 1),
		"local-root":          strings.Replace(valid, "cloud:backups", "/etc", 1),
		"traversal":           strings.Replace(valid, "cloud:backups", "cloud:../private", 1),
		"nested-traversal":    strings.Replace(valid, "cloud:backups", "cloud:a/../b", 1),
		"encoded-traversal":   strings.Replace(valid, "cloud:backups", "cloud:%2e%2e", 1),
		"absolute-root":       strings.Replace(valid, "cloud:backups", "cloud:/private", 1),
		"backslash":           strings.Replace(valid, "cloud:backups", "cloud:a\\b", 1),
		"inline-backend":      strings.Replace(valid, "cloud:backups", ":http,url=http://example.test:", 1),
		"cycle":               strings.Replace(valid, "cloud:backups", "encrypted:backups", 1),
		"unencrypted":         valid + "filename_encryption = off\n",
		"directories":         valid + "directory_name_encryption = false\n",
		"plain-password":      strings.Replace(valid, "password = ", "password = plaintext!", 1),
		"duplicate-key":       valid + "password = canary\n",
		"duplicate-section":   valid + "[encrypted]\n",
		"extra-section":       valid + "[another]\ntype = local\n",
		"global":              "[global]\ninsecure_skip_verify = true\n" + valid,
		"unknown-option":      valid + "password_command = malicious\n",
		"invalid-token":       strings.Replace(valid, "\"Bearer\"", "\"Basic\"", 1),
		"unknown-token-field": strings.Replace(valid, "\"token_type\"", "\"endpoint\":\"https://example.test\",\"token_type\"", 1),
		"missing-refresh":     strings.Replace(valid, "canary-refresh", "", 1),
		"invalid-expiry":      strings.Replace(valid, "2030-01-01T00:00:00Z", "invalid", 1),
		"nul":                 valid + "\x00",
		"invalid-utf8":        valid + string([]byte{255}),
		"oversized":           strings.Repeat("a", MaxConfigBytes+1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseConfig(raw, "encrypted"); err != ErrInvalidConfig {
				t.Fatal("unsafe configuration was not rejected with a safe error")
			}
		})
	}
	for _, raw := range []string{
		strings.Replace(valid, "cloud:backups", "cloud:other", 1),
		strings.Replace(valid, "example-drive", "other-drive", 1),
		strings.Replace(valid, base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{5}, 32)), base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{6}, 32)), 1),
	} {
		other, err := ParseConfig(raw, "encrypted")
		if err != nil {
			t.Fatal(err)
		}
		if config.SameTarget(other) {
			t.Fatal("changed target or crypt key allowed for replacement")
		}
	}
}

func FuzzParseConfig(f *testing.F) {
	f.Add(testConfig(), "encrypted")
	f.Add("", "encrypted")
	f.Fuzz(func(t *testing.T, raw, remote string) {
		config, err := ParseConfig(raw, remote)
		if err != nil {
			if err != ErrInvalidConfig {
				t.Fatal("parser returned unbounded diagnostics")
			}
			return
		}
		normalized := config.Bytes()
		defer clear(normalized)
		again, err := ParseConfig(string(normalized), remote)
		if err != nil || !config.SameTarget(again) {
			t.Fatal("normalization changed configuration")
		}
	})
}
