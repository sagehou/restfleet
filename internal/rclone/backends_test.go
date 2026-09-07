package rclone

import (
	"bytes"
	"encoding/base64"
	"net/netip"
	"strings"
	"testing"
)

func googleConfig() string {
	raw := strings.Replace(testConfig(), "type = onedrive", "type = drive", 1)
	return strings.Replace(raw, "drive_id = example-drive\ndrive_type = personal",
		"client_id = test-client.apps.googleusercontent.com\nclient_secret = test-client-secret\nscope = drive\nroot_folder_id = root-folder\nteam_drive = team-drive", 1)
}

func webDAVConfig() string {
	_, crypt, _ := strings.Cut(testConfig(), "[encrypted]")
	_, password, _ := strings.Cut(crypt, "password = ")
	return "[cloud]\ntype = webdav\nurl = https://dav.example.test/files/\nvendor = nextcloud\nuser = test-user\npass = " + strings.TrimSpace(password) + "\n[encrypted]" + crypt
}

func TestBackendConfigurationAndReplacement(t *testing.T) {
	for _, tc := range []struct{ backend, raw string }{{"onedrive", testConfig()}, {"drive", googleConfig()}, {"webdav", webDAVConfig()}} {
		t.Run(tc.backend, func(t *testing.T) {
			c, err := ParseConfig(tc.raw, "encrypted")
			if err != nil || c.Backend() != tc.backend {
				t.Fatal("backend not accepted")
			}
			raw := c.Bytes()
			defer clear(raw)
			again, err := ParseConfig(string(raw), "encrypted")
			if err != nil || !c.SameExceptToken(again) {
				t.Fatal("canonical round trip failed")
			}
		})
	}
	google, _ := ParseConfig(googleConfig(), "encrypted")
	for _, change := range [][2]string{{"canary-refresh", "next-refresh"}, {"test-client-secret", "rotated-secret"}, {"test-client.apps", "next-client.apps"}} {
		next, err := ParseConfig(strings.Replace(googleConfig(), change[0], change[1], 1), "encrypted")
		if err != nil || !google.SameTarget(next) {
			t.Fatal("same-target OAuth update denied")
		}
		if google.SameExceptToken(next) != (change[0] == "canary-refresh") {
			t.Fatal("watcher accepted non-token OAuth change")
		}
	}
	dav, _ := ParseConfig(webDAVConfig(), "encrypted")
	changedPassword := strings.Replace(webDAVConfig(), "pass = "+base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{5}, 32)),
		"pass = "+base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{6}, 32)), 1)
	next, err := ParseConfig(changedPassword, "encrypted")
	if err != nil || !dav.SameTarget(next) || dav.SameExceptToken(next) {
		t.Fatal("WebDAV password replacement/refresh rules incorrect")
	}
	for _, raw := range []string{
		strings.Replace(googleConfig(), "root-folder", "other-folder", 1),
		strings.Replace(googleConfig(), "team-drive", "other-team", 1),
		strings.Replace(googleConfig(), "scope = drive", "scope = drive.file", 1),
	} {
		next, err := ParseConfig(raw, "encrypted")
		if err != nil || google.SameTarget(next) {
			t.Fatal("Google storage target change accepted")
		}
	}
	for _, raw := range []string{
		strings.Replace(webDAVConfig(), "dav.example.test", "other.example.test", 1),
		strings.Replace(webDAVConfig(), "/files/", "/other/", 1),
		strings.Replace(webDAVConfig(), "test-user", "other-user", 1),
		strings.Replace(webDAVConfig(), "nextcloud", "other", 1),
	} {
		next, err := ParseConfig(raw, "encrypted")
		if err != nil || dav.SameTarget(next) {
			t.Fatal("WebDAV storage target or identity change accepted")
		}
	}
}

func TestBackendUnsafeOptions(t *testing.T) {
	for _, option := range []string{"service_account_file = /etc/passwd", "service_account_credentials = {}", "env_auth = true", "impersonate = somebody", "auth_url = https://evil.test", "token_url = http://169.254.169.254/", "scope = drive.readonly", "scope = drive.metadata.readonly"} {
		raw := strings.Replace(googleConfig(), "scope = drive", option, 1)
		if _, err := ParseConfig(raw, "encrypted"); err != ErrInvalidConfig {
			t.Fatal("unsafe Google option accepted")
		}
	}

	for _, raw := range []string{
		strings.ReplaceAll(googleConfig(), "client_id = test-client.apps.googleusercontent.com", "client_id = "),
		strings.ReplaceAll(strings.ReplaceAll(googleConfig(), "root_folder_id = root-folder", "root_folder_id = "), "team_drive = team-drive", "team_drive = "),
		strings.ReplaceAll(googleConfig(), "root_folder_id = root-folder", "root_folder_id = root"),
	} {
		if _, err := ParseConfig(raw, "encrypted"); err != ErrInvalidConfig {
			t.Fatal("Google identity or fixed target missing")
		}
	}
	for _, option := range []string{"unix_socket = /run/docker.sock", "bearer_token_command = malicious", "headers = Authorization,secret", "auth_redirect = true", "vendor = sharepoint-ntlm", "vendor = sharepoint", "insecure_skip_verify = true"} {
		raw := strings.Replace(webDAVConfig(), "vendor = nextcloud", option, 1)
		if _, err := ParseConfig(raw, "encrypted"); err != ErrInvalidConfig {
			t.Fatal("unsafe WebDAV option accepted")
		}
	}
	for _, endpoint := range []string{
		"http://dav.example.test/", "https://user:pass@dav.example.test/", "https://dav.example.test/?token=secret",
		"https://dav.example.test/#fragment", "https://dav.example.test//", "https://dav.example.test/a/../b", "https://dav.example.test/%2e%2e/private",
		"https://dav.example.test/%0apass=x", "https://dav.example.test:0/", "https://dav.example.test:65536/",
		"https://localhost/", "https://127.0.0.1/", "https://169.254.169.254/", "https://10.0.0.1/", "https://[::1]/",
		"https://[::ffff:127.0.0.1]/", "https://[fe80::1%25eth0]/", "https://168.63.129.16/",
	} {
		raw := strings.Replace(webDAVConfig(), "https://dav.example.test/files/", endpoint, 1)
		if _, err := ParseConfig(raw, "encrypted"); err != ErrInvalidConfig {
			t.Fatalf("unsafe endpoint accepted: %s", endpoint)
		}
	}
	c, _ := ParseConfig(webDAVConfig(), "encrypted")
	for _, raw := range []string{googleConfig(), testConfig()} {
		next, _ := ParseConfig(raw, "encrypted")
		if c.SameTarget(next) {
			t.Fatal("backend switch allowed as replacement")
		}
	}
	bearer := strings.Replace(webDAVConfig(), "user = test-user", "bearer_token = test-static-token", 1)
	at := strings.Index(bearer, "pass = ")
	end := strings.Index(bearer[at:], "\n") + at
	bearer = bearer[:at] + bearer[end+1:]
	c, err := ParseConfig(bearer, "encrypted")
	if err != nil {
		t.Fatal("static bearer rejected")
	}
	next, _ := ParseConfig(strings.Replace(bearer, "test-static-token", "next-token", 1), "encrypted")
	if !c.SameTarget(next) || c.SameExceptToken(next) {
		t.Fatal("bearer replacement mistaken for OAuth refresh")
	}
	basic, _ := ParseConfig(webDAVConfig(), "encrypted")
	if c.SameTarget(basic) {
		t.Fatal("authentication mode switched during replacement")
	}
}

func TestPublicIPClassification(t *testing.T) {
	for _, address := range []string{"0.0.0.0", "10.1.2.3", "100.100.100.200", "127.1.1.1", "169.254.1.1", "172.16.0.1", "192.168.0.1", "192.0.2.1", "198.18.0.1", "224.0.0.1", "255.255.255.255", "::", "::1", "fc00::1", "fe80::1", "::ffff:10.0.0.1", "64:ff9b::a00:1", "2002:a00:1::1", "2001:db8::1", "3fff::1"} {
		if publicIP(netip.MustParseAddr(address)) {
			t.Fatalf("non-public IP accepted: %s", address)
		}
	}
	for _, address := range []string{"93.184.216.34", "2606:4700:4700::1111", "::ffff:93.184.216.34"} {
		if !publicIP(netip.MustParseAddr(address)) {
			t.Fatal("public IP rejected")
		}
	}
}

func FuzzMultiBackendConfig(f *testing.F) {
	f.Add(googleConfig())
	f.Add(webDAVConfig())
	f.Fuzz(func(t *testing.T, raw string) {
		c, err := ParseConfig(raw, "encrypted")
		if err != nil {
			return
		}
		encoded := c.Bytes()
		defer clear(encoded)
		next, err := ParseConfig(string(encoded), "encrypted")
		if err != nil || !c.SameExceptToken(next) {
			t.Fatal("backend normalization unstable")
		}
	})
}
