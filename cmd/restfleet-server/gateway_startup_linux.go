package main

import (
	"context"
	"flag"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/sagehou/restfleet/internal/persistence/postgres"
	control "github.com/sagehou/restfleet/internal/server"
)

// Explicit one-shot central coordination: the regular Server does not replay
// startup files after a restart. No services, CA creation or subprocesses start.
func runGatewayStartup(args []string) error {
	if len(args) == 0 || args[0] != "gateway-start" {
		return control.ErrGatewayStartup
	}
	flags := flag.NewFlagSet("gateway-start", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	path := flags.String("config-file", "", "protected central startup metadata")
	if flags.Parse(args[1:]) != nil || flags.NArg() != 0 || *path == "" {
		return control.ErrGatewayStartup
	}
	startup, err := control.LoadGatewayStartupConfig(*path)
	if err != nil {
		return control.ErrGatewayStartup
	}
	config, err := control.LoadRuntimeConfig()
	defer clear(config.MasterKey)
	defer clear(config.GatewaySigningKey)
	defer clear(config.GatewayPendingDecryptionKey)
	if err != nil || !config.EnrollmentEnabled || len(config.GatewayPendingDecryptionKey) != 32 ||
		(config.Environment == "production" && (os.Geteuid() == 0 || startup.SharedGroup == 0)) {
		return control.ErrGatewayStartup
	}
	signalContext, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(signalContext, 10*time.Second)
	defer cancel()
	store, err := postgres.Open(ctx, config.DatabaseURL)
	if err != nil {
		return control.ErrGatewayStartup
	}
	defer store.Close()
	c, err := control.NewControlPlane(store, control.Settings{MasterKey: config.MasterKey, GatewayPublicURL: config.GatewayPublicURL,
		GatewaySigningKey: config.GatewaySigningKey, GatewayPendingDecryptionKey: config.GatewayPendingDecryptionKey,
		ExpectedSchema: postgres.ExpectedSchemaVersion, Enrollment: control.EnrollmentSettings{ServerCABundlePEM: config.ServerCABundlePEM}})
	if err != nil || c.InitializeGateway(ctx, startup) != nil {
		return control.ErrGatewayStartup
	}
	return nil
}
