package main

import (
	"context"
	"encoding/json"
	"flag"
	"io"
	"os/signal"
	"syscall"

	"github.com/sagehou/restfleet/internal/gatewaypending"
)

func runReplay(arguments []string, output io.Writer) error {
	flags := flag.NewFlagSet("replay", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	path := flags.String("config-file", "", "protected replay-only metadata")
	if flags.Parse(arguments) != nil || flags.NArg() != 0 || *path == "" {
		return gatewaypending.ErrReplayCommand
	}
	config, err := gatewaypending.LoadReplayConfig(*path)
	if err != nil {
		return gatewaypending.ErrReplayCommand
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	tail, err := gatewaypending.ReplayFromConfig(ctx, config)
	if err != nil || json.NewEncoder(output).Encode(tail) != nil {
		return gatewaypending.ErrReplayCommand
	}
	return nil
}
