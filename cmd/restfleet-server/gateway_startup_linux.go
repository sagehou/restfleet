package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

// Explicit central coordination: the regular Server never replays startup
// files. Optional follow hands the initialized admission to its sole worker;
// it never retries initialization, creates a CA or starts a subprocess.
func runGatewayCommand(args []string, output io.Writer) error {
	if len(args) == 0 {
		return control.ErrGatewayStartup
	}
	switch args[0] {
	case "gateway-rotate":
		return runGatewayRotateCommand(args[1:])
	case "gateway-recover":
		return runGatewayRecoverCommand(args[1:])
	}
	if args[0] != "gateway-start" && args[0] != "gateway-audit-register" && args[0] != "gateway-prepare" {
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
	var auditPath string
	var waitSeconds int
	var follow bool
	if prepare {
		flags.StringVar(&planPath, "plan-file", "", "durable protected startup plan")
	} else if !register {
		flags.StringVar(&auditPath, "audit-config-file", "", "protected global audit registration metadata")
		flags.IntVar(&waitSeconds, "wait-seconds", 0, "wait for the initial material socket, at most 300 seconds")
		flags.BoolVar(&follow, "follow", false, "continue as the sole authorization worker after initialization")
	}
	if flags.Parse(args[1:]) != nil || flags.NArg() != 0 || *path == "" || waitSeconds < 0 || waitSeconds > 300 {
		return failure
	}
	if prepare && (planPath == "" || planPath == *path) {
		return failure
	}
	var startup control.GatewayStartupConfig
	var audit control.GatewayAuditRegistrationConfig
	var preparation control.GatewayPreparation
	var targets []control.GatewayAuthorityTarget
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
	if auditPath != "" {
		audit, err = control.LoadGatewayAuditRegistrationConfig(auditPath)
		if err != nil || audit.AuditOrigin.RuntimeID != startup.Binding.RuntimeID || bytes.Equal(audit.SourcePublic, startup.SourcePublic) {
			return failure
		}
	}
	if follow {
		targets, err = control.LoadGatewayAuthorityTargets(*path)
		if err != nil || len(targets) != 1 || targets[0].Binding != startup.Binding ||
			!bytes.Equal(targets[0].SourcePublic, startup.SourcePublic) || targets[0].GatewayUID != startup.GatewayUID ||
			targets[0].SharedGroup != startup.SharedGroup || targets[0].SocketPath == startup.SocketPath {
			return failure
		}
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
		(follow && config.GatewayAuthorityFile != "") ||
		(config.Environment == "production" && (os.Geteuid() == 0 || (!register && sharedGroup == 0))) {
		return failure
	}
	signalContext, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(signalContext, time.Duration(waitSeconds)*time.Second+10*time.Second)
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
	} else {
		if auditPath != "" {
			if _, err := c.RegisterGatewayAuditFromConfig(ctx, audit); err != nil {
				return failure
			}
		}
		if waitGatewayMaterialSocket(ctx, startup.SocketPath, time.Duration(waitSeconds)*time.Second) != nil ||
			c.InitializeGateway(ctx, startup) != nil {
			return failure
		}
		if follow {
			cancel() // Setup has finished; the worker owns only the signal context.
			logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
			return c.RunGatewayAuthorityWorker(signalContext, targets, func() {
				logger.Warn("gateway authorization retry pending", "component", "gateway_authority", "event", "delivery_retry")
			})
		}
	}
	return nil
}

// Wait only for first publication. An existing socket, refused exchange or
// material intent is never retried/replaced. InitializeGateway still performs
// all protected-path, exact peer UID and source-challenge authentication.
func waitGatewayMaterialSocket(ctx context.Context, path string, wait time.Duration) error {
	if wait == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return control.ErrGatewayStartup
		}
		info, err := os.Lstat(path)
		if err == nil {
			if info.Mode()&os.ModeSocket == 0 {
				return control.ErrGatewayStartup
			}
			return nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return control.ErrGatewayStartup
		}
		select {
		case <-ctx.Done():
			return control.ErrGatewayStartup
		case <-ticker.C:
		}
	}
}
