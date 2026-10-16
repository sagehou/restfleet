package main

import (
	"context"
	"encoding/json"
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
func runGatewayCommand(args []string, output io.Writer) error {
	if len(args) == 0 || (args[0] != "gateway-start" && args[0] != "gateway-audit-register" && args[0] != "gateway-prepare") {
		return control.ErrGatewayStartup
	}
	failure := control.ErrGatewayStartup
	register := args[0] == "gateway-audit-register"
	prepare := args[0] == "gateway-prepare"
	if register {
		failure = control.ErrGatewayAuditRegistration
		if output == nil {
			return failure
		}
	}
	flags := flag.NewFlagSet(args[0], flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	path := flags.String("config-file", "", "protected central startup metadata")
	var planPath string
	if prepare {
		flags.StringVar(&planPath, "plan-file", "", "durable protected startup plan")
	}
	if flags.Parse(args[1:]) != nil || flags.NArg() != 0 || *path == "" {
		return failure
	}
	if prepare && (planPath == "" || planPath == *path) {
		return failure
	}
	var startup control.GatewayStartupConfig
	var audit control.GatewayAuditRegistrationConfig
	var preparation control.GatewayPreparation
	var err error
	if register {
		audit, err = control.LoadGatewayAuditRegistrationConfig(*path)
	} else if prepare {
		preparation, err = control.LoadGatewayPreparation(*path)
	} else {
		startup, err = control.LoadGatewayStartupConfig(*path)
	}
	if err != nil {
		return failure
	}
	config, err := control.LoadRuntimeConfig()
	defer clear(config.MasterKey)
	defer clear(config.GatewaySigningKey)
	defer clear(config.GatewayPendingDecryptionKey)
	sharedGroup := startup.SharedGroup
	if prepare {
		sharedGroup = preparation.SharedGroup
	}
	if err != nil || !config.EnrollmentEnabled || len(config.GatewayPendingDecryptionKey) != 32 ||
		(config.Environment == "production" && (os.Geteuid() == 0 || (!register && sharedGroup == 0))) {
		return failure
	}
	signalContext, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(signalContext, 10*time.Second)
	defer cancel()
	store, err := postgres.Open(ctx, config.DatabaseURL)
	if err != nil {
		return failure
	}
	defer store.Close()
	c, err := control.NewControlPlane(store, control.Settings{MasterKey: config.MasterKey, GatewayPublicURL: config.GatewayPublicURL,
		GatewayOfflineWindow: config.GatewayOfflineWindow,
		GatewaySigningKey: config.GatewaySigningKey, GatewayPendingDecryptionKey: config.GatewayPendingDecryptionKey,
		ExpectedSchema: postgres.ExpectedSchemaVersion, Enrollment: control.EnrollmentSettings{ServerCABundlePEM: config.ServerCABundlePEM}})
	if err != nil {
		return failure
	}
	if register {
		result, err := c.RegisterGatewayAuditFromConfig(ctx, audit)
		if err != nil || json.NewEncoder(output).Encode(result) != nil {
			return failure
		}
	} else if prepare {
		if c.PrepareGateway(ctx, preparation, planPath) != nil {
			return failure
		}
	} else if c.InitializeGateway(ctx, startup) != nil {
		return failure
	}
	return nil
}
