package main

import (
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	agentv1 "github.com/sagehou/restfleet/api/proto/gen/go/restfleet/agent/v1"
	"github.com/sagehou/restfleet/internal/buildinfo"
	"github.com/sagehou/restfleet/internal/persistence/postgres"
	"github.com/sagehou/restfleet/internal/security"
	control "github.com/sagehou/restfleet/internal/server"
	"github.com/sagehou/restfleet/internal/server/agentgrpc"
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

	var agentCA *security.AgentCA
	if config.EnrollmentEnabled {
		agentCA, err = control.LoadOrCreateAgentCA(ctx, store, config.MasterKey, time.Now().UTC())
		if err != nil {
			return err
		}
	}
	controlPlane, err := control.NewControlPlane(store, control.Settings{
		BootstrapToken: config.BootstrapToken,
		ExpectedSchema: postgres.ExpectedSchemaVersion,
		Enrollment: control.EnrollmentSettings{
			Pepper: security.DeriveEnrollmentPepper(config.MasterKey),
			CA:     agentCA, PublicURL: config.PublicURL,
			GRPCEndpoint: config.GRPCEndpoint, ServerName: config.GRPCServerName,
			ServerCABundlePEM: config.ServerCABundlePEM,
			HeartbeatInterval: 15 * time.Second,
		},
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

	var grpcServer *grpc.Server
	var grpcListener net.Listener
	if config.EnrollmentEnabled {
		serverCertificate, err := tls.LoadX509KeyPair(config.GRPCTLSCertFile, config.GRPCTLSKeyFile)
		if err != nil {
			return err
		}
		tlsConfig := &tls.Config{
			MinVersion:   tls.VersionTLS12,
			Certificates: []tls.Certificate{serverCertificate},
			ClientAuth:   tls.RequireAndVerifyClientCert,
			ClientCAs:    agentCA.CertPool(),
		}
		grpcListener, err = net.Listen("tcp", config.GRPCAddress)
		if err != nil {
			return err
		}
		defer grpcListener.Close()
		grpcServer = grpc.NewServer(
			grpc.Creds(credentials.NewTLS(tlsConfig)),
			grpc.MaxRecvMsgSize(1<<20),
			grpc.MaxSendMsgSize(1<<20),
		)
		agentService := agentgrpc.New(controlPlane, config.ServerCABundlePEM, 15*time.Second)
		controlPlane.SetAgentDisconnector(agentService.DisconnectAgent)
		agentv1.RegisterAgentControlServiceServer(grpcServer, agentService)
	}

	serverErrors := make(chan error, 3)
	go func() {
		logger.Info("public listener started", "component", "server", "event", "listener_started", "address", config.HTTPAddress)
		if config.EnrollmentEnabled {
			serverErrors <- publicServer.ListenAndServeTLS(config.GRPCTLSCertFile, config.GRPCTLSKeyFile)
			return
		}
		serverErrors <- publicServer.ListenAndServe()
	}()
	go func() {
		logger.Info("metrics listener started", "component", "server", "event", "listener_started", "address", config.MetricsAddress)
		serverErrors <- metricsServer.ListenAndServe()
	}()

	if grpcServer != nil {
		go func() {
			logger.Info("Agent gRPC listener started", "component", "server", "event", "listener_started", "address", config.GRPCAddress)
			serverErrors <- grpcServer.Serve(grpcListener)
		}()
	}

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
	if grpcServer != nil {
		stopped := make(chan struct{})
		go func() {
			grpcServer.GracefulStop()
			close(stopped)
		}()
		select {
		case <-stopped:
		case <-shutdownContext.Done():
			grpcServer.Stop()
		}
	}
	return errors.Join(serveErr, publicErr, metricsErr)
}
