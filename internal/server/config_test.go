package server

import (
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
