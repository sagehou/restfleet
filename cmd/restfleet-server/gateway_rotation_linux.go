package main

import (
	"context"
	"flag"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/sagehou/restfleet/internal/persistence/postgres"
	control "github.com/sagehou/restfleet/internal/server"
)

func runGatewayRotateCommand(args []string) error {
	flags := flag.NewFlagSet("gateway-rotate", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	path := flags.String("config-file", "", "protected central rotation metadata")
	if flags.Parse(args) != nil || flags.NArg() != 0 || *path == "" {
		return control.ErrGatewayRotation
	}
	rotation, err := control.LoadGatewayRotationConfig(*path)
	if err != nil {
		return control.ErrGatewayRotation
	}
	config, err := control.LoadRuntimeConfig()
	defer clear(config.MasterKey)
	defer clear(config.GatewaySigningKey)
	defer clear(config.GatewayPendingDecryptionKey)
	if err != nil || !config.EnrollmentEnabled || config.GatewayAuthorityFile != "" ||
		len(config.GatewaySigningKey) == 0 || len(config.GatewayPendingDecryptionKey) != 32 ||
		(config.Environment == "production" && (os.Geteuid() == 0 || rotation.Next.SharedGroup == 0)) {
		return control.ErrGatewayRotation
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	store, plane, err := openGatewayCommandControlPlane(ctx, config)
	if err != nil {
		return control.ErrGatewayRotation
	}
	defer store.Close()
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	return plane.RotateGateway(ctx, rotation, func() {
		logger.Warn("gateway rotation authorization retry pending", "component", "gateway_rotation", "event", "delivery_retry")
	})
}

func runGatewayRecoverCommand(args []string) error {
	flags := flag.NewFlagSet("gateway-recover", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	path := flags.String("config-file", "", "protected central recovery metadata")
	if flags.Parse(args) != nil || flags.NArg() != 0 || *path == "" {
		return control.ErrGatewayRecovery
	}
	recovery, err := control.LoadGatewayRecoveryConfig(*path)
	if err != nil {
		return control.ErrGatewayRecovery
	}
	config, err := control.LoadRuntimeConfig()
	defer clear(config.MasterKey)
	defer clear(config.GatewaySigningKey)
	defer clear(config.GatewayPendingDecryptionKey)
	if err != nil || !config.EnrollmentEnabled || config.GatewayAuthorityFile != "" ||
		len(config.GatewaySigningKey) == 0 || len(config.GatewayPendingDecryptionKey) != 32 ||
		(config.Environment == "production" && os.Geteuid() == 0) {
		return control.ErrGatewayRecovery
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	store, plane, err := openGatewayCommandControlPlane(ctx, config)
	if err != nil {
		return control.ErrGatewayRecovery
	}
	defer store.Close()
	return plane.RecoverGateway(ctx, recovery)
}

func openGatewayCommandControlPlane(ctx context.Context, config control.RuntimeConfig) (*postgres.Store, *control.ControlPlane, error) {
	store, err := postgres.Open(ctx, config.DatabaseURL)
	if err != nil {
		return nil, nil, err
	}
	plane, err := control.NewControlPlane(store, control.Settings{
		MasterKey:                   config.MasterKey,
		GatewayPublicURL:            config.GatewayPublicURL,
		GatewayOfflineWindow:        config.GatewayOfflineWindow,
		GatewaySigningKey:           config.GatewaySigningKey,
		GatewayPendingDecryptionKey: config.GatewayPendingDecryptionKey,
		ExpectedSchema:              postgres.ExpectedSchemaVersion,
		Enrollment: control.EnrollmentSettings{ServerCABundlePEM: config.ServerCABundlePEM},
	})
	if err != nil {
		store.Close()
		return nil, nil, err
	}
	return store, plane, nil
}
