package server

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

type RuntimeConfig struct {
	Environment       string
	DatabaseURL       string
	BootstrapToken    string
	HTTPAddress       string
	MetricsAddress    string
	WebDirectory      string
	SecureCookies     bool
	EnrollmentEnabled bool
	MasterKey         []byte
	PublicURL         string
	GRPCAddress       string
	GRPCEndpoint      string
	GRPCServerName    string
	GRPCTLSCertFile   string
	GRPCTLSKeyFile    string
	ServerCABundlePEM []byte
	Warnings          []string
}

func LoadRuntimeConfig() (RuntimeConfig, error) {
	config := RuntimeConfig{
		Environment:    strings.ToLower(envOrDefault("RESTFLEET_ENV", "production")),
		HTTPAddress:    envOrDefault("RESTFLEET_HTTP_ADDRESS", ":8080"),
		MetricsAddress: envOrDefault("RESTFLEET_METRICS_ADDRESS", "127.0.0.1:9090"),
		WebDirectory:   envOrDefault("RESTFLEET_WEB_DIR", "/srv/restfleet/web"),
		GRPCAddress:    envOrDefault("RESTFLEET_GRPC_ADDRESS", ":8443"),
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

	config.PublicURL = strings.TrimSpace(os.Getenv("RESTFLEET_PUBLIC_URL"))
	config.GRPCEndpoint = strings.TrimSpace(os.Getenv("RESTFLEET_GRPC_ENDPOINT"))
	config.GRPCServerName = strings.TrimSpace(os.Getenv("RESTFLEET_GRPC_SERVER_NAME"))
	config.GRPCTLSCertFile = strings.TrimSpace(os.Getenv("RESTFLEET_GRPC_TLS_CERT_FILE"))
	config.GRPCTLSKeyFile = strings.TrimSpace(os.Getenv("RESTFLEET_GRPC_TLS_KEY_FILE"))
	serverCAFile := strings.TrimSpace(os.Getenv("RESTFLEET_SERVER_CA_BUNDLE_FILE"))
	masterKeyEncoded, keyErr := readSecretSetting("RESTFLEET_MASTER_KEY", config.Environment, &config.Warnings)
	if keyErr != nil {
		return RuntimeConfig{}, keyErr
	}
	enrollmentValues := []string{
		config.PublicURL, config.GRPCEndpoint, config.GRPCServerName,
		config.GRPCTLSCertFile, config.GRPCTLSKeyFile, serverCAFile, masterKeyEncoded,
	}
	for _, value := range enrollmentValues {
		if value != "" {
			config.EnrollmentEnabled = true
			break
		}
	}
	if config.EnrollmentEnabled {
		for _, value := range enrollmentValues {
			if value == "" {
				return RuntimeConfig{}, errors.New("all Agent enrollment and gRPC settings are required when enrollment is enabled")
			}
		}
		if err := ValidateEnrollmentPublicURL(config.PublicURL); err != nil {
			return RuntimeConfig{}, err
		}
		config.MasterKey, err = base64.StdEncoding.DecodeString(strings.TrimSpace(masterKeyEncoded))
		if err != nil || len(config.MasterKey) != 32 {
			return RuntimeConfig{}, errors.New("RESTFLEET_MASTER_KEY_FILE must contain base64 for exactly 32 bytes")
		}
		config.ServerCABundlePEM, err = os.ReadFile(serverCAFile)
		if err != nil {
			return RuntimeConfig{}, fmt.Errorf("read RESTFLEET_SERVER_CA_BUNDLE_FILE: %w", err)
		}
		if !strings.Contains(string(config.ServerCABundlePEM), "-----BEGIN CERTIFICATE-----") {
			return RuntimeConfig{}, errors.New("RESTFLEET_SERVER_CA_BUNDLE_FILE is not a PEM certificate bundle")
		}
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
