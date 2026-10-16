package main

import (
	"context"
	"flag"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/sagehou/restfleet/internal/gateway"
)

func runService(arguments []string) error {
	flags := flag.NewFlagSet("run", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	path := flags.String("config-file", "", "protected Gateway service configuration")
	if flags.Parse(arguments) != nil || flags.NArg() != 0 || *path == "" {
		return gateway.ErrService
	}
	config, err := gateway.LoadServiceConfig(*path)
	if err != nil || config.ClientCAFile == "" {
		return gateway.ErrService
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	return gateway.RunService(ctx, config)
}
