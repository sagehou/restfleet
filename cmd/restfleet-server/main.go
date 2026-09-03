package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/sagehou/restfleet/internal/buildinfo"
	"github.com/sagehou/restfleet/internal/persistence/postgres"
	control "github.com/sagehou/restfleet/internal/server"
	"github.com/sagehou/restfleet/internal/server/httpapi"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err := run(logger); err != nil {
		logger.Error("server stopped", "component", "server", "event", "server_exit", "error_code", "SERVER_EXIT")
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	config, err := control.LoadRuntimeConfig()
	if err != nil {
		return err
	}
	for _, warning := range config.Warnings {
		logger.Warn(warning, "component", "server", "event", "insecure_development_configuration")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	store, err := postgres.Open(ctx, config.DatabaseURL)
	if err != nil {
		return err
	}
	defer store.Close()

	required, bootstrapErr := store.BootstrapRequired(ctx)
	if bootstrapErr == nil && required && config.BootstrapToken == "" {
		return errors.New("bootstrap token is required until the first administrator is created")
	}

	controlPlane, err := control.NewControlPlane(store, control.Settings{
		BootstrapToken: config.BootstrapToken,
		ExpectedSchema: postgres.ExpectedSchemaVersion,
	})
	if err != nil {
		return err
	}
	api := httpapi.New(controlPlane, httpapi.Options{
		SecureCookies: config.SecureCookies,
		StaticDir:     config.WebDirectory,
		Logger:        logger,
		Build: httpapi.BuildInfo{
			Version: buildinfo.Version,
			Commit:  buildinfo.Commit,
			Date:    buildinfo.Date,
		},
	})

	publicServer := &http.Server{
		Addr:              config.HTTPAddress,
		Handler:           api.NewRootHandler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    32 << 10,
	}
	metricsMux := http.NewServeMux()
	metricsMux.Handle("GET /metrics", api.MetricsHandler())
	metricsServer := &http.Server{
		Addr:              config.MetricsAddress,
		Handler:           metricsMux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       30 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}

	serverErrors := make(chan error, 2)
	go func() {
		logger.Info("public listener started", "component", "server", "event", "listener_started", "address", config.HTTPAddress)
		serverErrors <- publicServer.ListenAndServe()
	}()
	go func() {
		logger.Info("metrics listener started", "component", "server", "event", "listener_started", "address", config.MetricsAddress)
		serverErrors <- metricsServer.ListenAndServe()
	}()

	var serveErr error
	select {
	case <-ctx.Done():
	case err := <-serverErrors:
		if !errors.Is(err, http.ErrServerClosed) {
			serveErr = err
			stop()
		}
	}

	shutdownContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	publicErr := publicServer.Shutdown(shutdownContext)
	metricsErr := metricsServer.Shutdown(shutdownContext)
	return errors.Join(serveErr, publicErr, metricsErr)
}
