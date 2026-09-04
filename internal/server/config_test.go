package server

import (
	"encoding/base64"
	"os"
	"path/filepath"
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
