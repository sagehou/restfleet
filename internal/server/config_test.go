package server

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func clearEnvironment(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"RESTFLEET_ENV",
		"RESTFLEET_DATABASE_URL",
		"RESTFLEET_DATABASE_URL_FILE",
		"RESTFLEET_BOOTSTRAP_TOKEN",
		"RESTFLEET_BOOTSTRAP_TOKEN_FILE",
		"RESTFLEET_SECURE_COOKIES",
		"RESTFLEET_HTTP_ADDRESS",
		"RESTFLEET_METRICS_ADDRESS",
		"RESTFLEET_WEB_DIR",
		"RESTFLEET_MASTER_KEY",
		"RESTFLEET_MASTER_KEY_FILE",
		"RESTFLEET_PUBLIC_URL",
		"RESTFLEET_GATEWAY_PUBLIC_URL",
		"RESTFLEET_GATEWAY_SIGNING_KEY",
		"RESTFLEET_GATEWAY_SIGNING_KEY_FILE",
		"RESTFLEET_GATEWAY_PENDING_KEY",
		"RESTFLEET_GATEWAY_PENDING_KEY_FILE",
		"RESTFLEET_GATEWAY_REPLAY_SOCKET",
		"RESTFLEET_GATEWAY_REPLAY_PEER_UID",
		"RESTFLEET_GATEWAY_REPLAY_GROUP",
		"RESTFLEET_GRPC_ADDRESS",
		"RESTFLEET_GRPC_ENDPOINT",
		"RESTFLEET_GRPC_SERVER_NAME",
		"RESTFLEET_GRPC_TLS_CERT_FILE",
		"RESTFLEET_GRPC_TLS_KEY_FILE",
		"RESTFLEET_SERVER_CA_BUNDLE_FILE",
	} {
		value, present := os.LookupEnv(name)
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if present {
				_ = os.Setenv(name, value)
			} else {
				_ = os.Unsetenv(name)
			}
		})
	}
}

func TestRuntimeConfigRejectsProductionSecretEnvironment(t *testing.T) {
	clearEnvironment(t)
	t.Setenv("RESTFLEET_ENV", "production")
	t.Setenv("RESTFLEET_DATABASE_URL", "postgres://must-not-be-used")
	_, err := LoadRuntimeConfig()
	if err == nil || !strings.Contains(err.Error(), "DATABASE_URL_FILE") {
		t.Fatalf("production environment secret was not rejected: %v", err)
	}
}

func TestRuntimeConfigAllowsExplicitDevelopmentMode(t *testing.T) {
	clearEnvironment(t)
	t.Setenv("RESTFLEET_ENV", "development")
	t.Setenv("RESTFLEET_DATABASE_URL", "postgres://development-only")
	t.Setenv("RESTFLEET_BOOTSTRAP_TOKEN", "development-only-token")
	t.Setenv("RESTFLEET_SECURE_COOKIES", "false")
	config, err := LoadRuntimeConfig()
	if err != nil {
		t.Fatal(err)
	}
	if config.SecureCookies || len(config.Warnings) != 3 {
		t.Fatalf("development warnings or cookie policy incorrect: %+v", config)
	}
}

func TestRuntimeConfigReadsProductionSecretFiles(t *testing.T) {
	clearEnvironment(t)
	directory := t.TempDir()
	databaseFile := filepath.Join(directory, "database-url")
	if err := os.WriteFile(databaseFile, []byte("postgres://from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RESTFLEET_ENV", "production")
	t.Setenv("RESTFLEET_DATABASE_URL_FILE", databaseFile)
	config, err := LoadRuntimeConfig()
	if err != nil {
		t.Fatal(err)
	}
	if config.DatabaseURL != "postgres://from-file" || len(config.Warnings) != 0 || !config.SecureCookies {
		t.Fatalf("production file configuration incorrect: %+v", config)
	}
}

func TestRuntimeConfigRejectsPartialEnrollmentConfiguration(t *testing.T) {
	clearEnvironment(t)
	t.Setenv("RESTFLEET_ENV", "development")
	t.Setenv("RESTFLEET_DATABASE_URL", "postgres://development-only")
	t.Setenv("RESTFLEET_PUBLIC_URL", "https://control.example")
	if _, err := LoadRuntimeConfig(); err == nil || !strings.Contains(err.Error(), "all Agent enrollment") {
		t.Fatalf("partial enrollment configuration was accepted: %v", err)
	}
}

func TestRuntimeConfigLoadsEnrollmentSecretsFromFiles(t *testing.T) {
	clearEnvironment(t)
	directory := t.TempDir()
	masterKeyFile := filepath.Join(directory, "master-key")
	serverCAFile := filepath.Join(directory, "server-ca.pem")
	if err := os.WriteFile(masterKeyFile, []byte(base64.StdEncoding.EncodeToString(make([]byte, 32))), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(serverCAFile, []byte("-----BEGIN CERTIFICATE-----\ntest\n-----END CERTIFICATE-----\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RESTFLEET_ENV", "development")
	t.Setenv("RESTFLEET_DATABASE_URL", "postgres://development-only")
	t.Setenv("RESTFLEET_MASTER_KEY_FILE", masterKeyFile)
	t.Setenv("RESTFLEET_PUBLIC_URL", "https://control.example")
	t.Setenv("RESTFLEET_GRPC_ENDPOINT", "control.example:443")
	t.Setenv("RESTFLEET_GRPC_SERVER_NAME", "control.example")
	t.Setenv("RESTFLEET_GRPC_TLS_CERT_FILE", filepath.Join(directory, "server.crt"))
	t.Setenv("RESTFLEET_GRPC_TLS_KEY_FILE", filepath.Join(directory, "server.key"))
	t.Setenv("RESTFLEET_SERVER_CA_BUNDLE_FILE", serverCAFile)
	config, err := LoadRuntimeConfig()
	if err != nil {
		t.Fatal(err)
	}
	if !config.EnrollmentEnabled || len(config.MasterKey) != 32 ||
		config.GRPCEndpoint != "control.example:443" {
		t.Fatalf("enrollment configuration incorrect: %+v", config)
	}
}

func TestGatewayOriginRequiresEnrollment(t *testing.T) {
	clearEnvironment(t)
	t.Setenv("RESTFLEET_ENV", "development")
	t.Setenv("RESTFLEET_DATABASE_URL", "postgres://development-only")
	t.Setenv("RESTFLEET_GATEWAY_PUBLIC_URL", "https://gateway.example")
	if _, err := LoadRuntimeConfig(); err == nil {
		t.Fatal("Gateway configured without authenticated enrollment")
	}
}

func TestGatewayKeysAreFileOnlyAndReplayRequiresBothKeys(t *testing.T) {
	clearEnvironment(t)
	directory := t.TempDir()
	write := func(name, value string) string {
		path := filepath.Join(directory, name)
		if err := os.WriteFile(path, []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	t.Setenv("RESTFLEET_ENV", "production")
	t.Setenv("RESTFLEET_DATABASE_URL_FILE", write("database", "postgres://test-only"))
	t.Setenv("RESTFLEET_MASTER_KEY_FILE", write("master", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{8}, 32))))
	t.Setenv("RESTFLEET_PUBLIC_URL", "https://control.example")
	t.Setenv("RESTFLEET_GATEWAY_PUBLIC_URL", "https://gateway.example")
	t.Setenv("RESTFLEET_GRPC_ENDPOINT", "control.example:443")
	t.Setenv("RESTFLEET_GRPC_SERVER_NAME", "control.example")
	t.Setenv("RESTFLEET_GRPC_TLS_CERT_FILE", filepath.Join(directory, "server.crt"))
	t.Setenv("RESTFLEET_GRPC_TLS_KEY_FILE", filepath.Join(directory, "server.key"))
	t.Setenv("RESTFLEET_SERVER_CA_BUNDLE_FILE", write("ca", "-----BEGIN CERTIFICATE-----\ntest\n-----END CERTIFICATE-----\n"))
	seed := bytes.Repeat([]byte{9}, 32)
	signingFile := write("signing", base64.StdEncoding.EncodeToString(seed))
	pendingFile := write("pending", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{10}, 32)))
	t.Setenv("RESTFLEET_GATEWAY_REPLAY_SOCKET", filepath.Join(directory, "replay.sock"))
	if _, err := LoadRuntimeConfig(); err == nil {
		t.Fatal("unsigned replay enabled")
	}
	t.Setenv("RESTFLEET_GATEWAY_PENDING_KEY_FILE", pendingFile)
	if _, err := LoadRuntimeConfig(); err == nil {
		t.Fatal("pending without signing key enabled")
	}
	t.Setenv("RESTFLEET_GATEWAY_SIGNING_KEY_FILE", signingFile)
	config, err := LoadRuntimeConfig()
	if err != nil || !bytes.Equal(config.GatewaySigningKey, ed25519.NewKeyFromSeed(seed)) || len(config.GatewayPendingDecryptionKey) != 32 || config.GatewayReplaySocket == "" {
		t.Fatal("valid protected configuration")
	}
	if config.GatewayReplayPeerUID != uint32(os.Geteuid()) || config.GatewayReplayGroup != 0 {
		t.Fatal("private replay default changed")
	}
	for _, invalid := range []struct{ peer, group string }{
		{"", "65530"}, {"65532", ""}, {"0", "65530"}, {"65532", "0"},
		{"-1", "65530"}, {"65532", "4294967295"}, {"4294967296", "65530"},
		{"065532", "65530"}, {"65532", "+65530"},
		{strconv.Itoa(os.Geteuid()), "65530"}, {"uid-secret-canary", "65530"},
	} {
		t.Run("invalid shared replay "+invalid.peer+"/"+invalid.group, func(t *testing.T) {
			t.Setenv("RESTFLEET_GATEWAY_REPLAY_PEER_UID", invalid.peer)
			t.Setenv("RESTFLEET_GATEWAY_REPLAY_GROUP", invalid.group)
			if _, err := LoadRuntimeConfig(); err == nil || strings.Contains(err.Error(), "canary") {
				t.Fatal("invalid shared replay accepted or echoed")
			}
		})
	}
	t.Run("explicit shared replay", func(t *testing.T) {
		peer := uint32(65532)
		if peer == uint32(os.Geteuid()) {
			peer++
		}
		t.Setenv("RESTFLEET_GATEWAY_REPLAY_PEER_UID", strconv.FormatUint(uint64(peer), 10))
		t.Setenv("RESTFLEET_GATEWAY_REPLAY_GROUP", "65530")
		shared, err := LoadRuntimeConfig()
		if err != nil || shared.GatewayReplayPeerUID != peer || shared.GatewayReplayGroup != 65530 {
			t.Fatal("explicit shared replay configuration rejected")
		}
		t.Setenv("RESTFLEET_GATEWAY_REPLAY_SOCKET", "")
		if _, err := LoadRuntimeConfig(); err == nil {
			t.Fatal("shared replay without socket accepted")
		}
	})
	for _, name := range []string{"RESTFLEET_GATEWAY_SIGNING_KEY", "RESTFLEET_GATEWAY_PENDING_KEY"} {
		t.Setenv(name, "key-secret-canary")
		if _, err := LoadRuntimeConfig(); err == nil || strings.Contains(err.Error(), "canary") {
			t.Fatal("environment key accepted/echoed")
		}
		if os.Unsetenv(name) != nil {
			t.Fatal("unset")
		}
	}
	if os.Chmod(signingFile, 0644) != nil {
		t.Fatal("chmod")
	}
	if _, err := LoadRuntimeConfig(); err == nil {
		t.Fatal("world-readable signing key loaded")
	}
}
