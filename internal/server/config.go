package server

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

type RuntimeConfig struct {
	Environment    string
	DatabaseURL    string
	BootstrapToken string
	HTTPAddress    string
	MetricsAddress string
	WebDirectory   string
	SecureCookies  bool
	Warnings       []string
}

func LoadRuntimeConfig() (RuntimeConfig, error) {
	config := RuntimeConfig{
		Environment:    strings.ToLower(envOrDefault("RESTFLEET_ENV", "production")),
		HTTPAddress:    envOrDefault("RESTFLEET_HTTP_ADDRESS", ":8080"),
		MetricsAddress: envOrDefault("RESTFLEET_METRICS_ADDRESS", "127.0.0.1:9090"),
		WebDirectory:   envOrDefault("RESTFLEET_WEB_DIR", "/srv/restfleet/web"),
		SecureCookies:  true,
	}
	if config.Environment != "production" && config.Environment != "development" && config.Environment != "test" {
		return RuntimeConfig{}, errors.New("RESTFLEET_ENV must be production, development, or test")
	}
	if config.Environment != "production" && os.Getenv("RESTFLEET_WEB_DIR") == "" {
		config.WebDirectory = "web/dist"
	}

	var err error
	config.DatabaseURL, err = readSecretSetting("RESTFLEET_DATABASE_URL", config.Environment, &config.Warnings)
	if err != nil {
		return RuntimeConfig{}, err
	}
	if config.DatabaseURL == "" {
		return RuntimeConfig{}, errors.New("database URL is required")
	}
	config.BootstrapToken, err = readSecretSetting("RESTFLEET_BOOTSTRAP_TOKEN", config.Environment, &config.Warnings)
	if err != nil {
		return RuntimeConfig{}, err
	}

	if value := os.Getenv("RESTFLEET_SECURE_COOKIES"); value != "" {
		config.SecureCookies, err = strconv.ParseBool(value)
		if err != nil {
			return RuntimeConfig{}, errors.New("RESTFLEET_SECURE_COOKIES must be a boolean")
		}
	}
	if !config.SecureCookies {
		if config.Environment == "production" {
			return RuntimeConfig{}, errors.New("secure cookies cannot be disabled in production")
		}
		config.Warnings = append(config.Warnings, "secure cookies are disabled")
	}
	return config, nil
}

func readSecretSetting(name, environment string, warnings *[]string) (string, error) {
	fileName := os.Getenv(name + "_FILE")
	environmentValue, hasEnvironmentValue := os.LookupEnv(name)
	if fileName != "" && hasEnvironmentValue {
		return "", fmt.Errorf("%s and %s_FILE cannot both be set", name, name)
	}
	if fileName != "" {
		value, err := os.ReadFile(fileName)
		if err != nil {
			return "", fmt.Errorf("read %s_FILE: %w", name, err)
		}
		return strings.TrimSuffix(strings.TrimSuffix(string(value), "\n"), "\r"), nil
	}
	if hasEnvironmentValue {
		if environment == "production" {
			return "", fmt.Errorf("%s may only be supplied through %s_FILE in production", name, name)
		}
		*warnings = append(*warnings, name+" is supplied through a development-only environment variable")
		return environmentValue, nil
	}
	return "", nil
}

func envOrDefault(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
