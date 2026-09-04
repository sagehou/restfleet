package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/sagehou/restfleet/internal/agent"
	"github.com/sagehou/restfleet/internal/buildinfo"
	"github.com/sagehou/restfleet/internal/security"
)

const defaultStateDirectory = "/var/lib/restfleet-agent"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, security.Redact(err.Error()))
		os.Exit(1)
	}
}

func run() error {
	command := "run"
	arguments := os.Args[1:]
	if len(arguments) > 0 && (arguments[0] == "enroll" || arguments[0] == "run" || arguments[0] == "version") {
		command = arguments[0]
		arguments = arguments[1:]
	}
	if command == "version" {
		fmt.Printf("restfleet-agent %s\n", buildinfo.String())
		return nil
	}
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	stateDirectory := flags.String("state-dir", defaultStateDirectory, "durable Agent state directory")
	if command == "enroll" {
		serverURL := flags.String("server", "", "RestFleet HTTPS base URL")
		tokenFile := flags.String("token-file", "", "file or stdin path containing the one-time token")
		caFile := flags.String("ca-file", "", "optional private CA bundle for initial HTTPS enrollment")
		if err := flags.Parse(arguments); err != nil {
			return err
		}
		if *serverURL == "" || *tokenFile == "" {
			return errors.New("--server and --token-file are required")
		}
		state, err := agent.OpenState(*stateDirectory)
		if err != nil {
			return err
		}
		defer state.Close()
		ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer cancel()
		identity, err := agent.Enroll(ctx, state, agent.EnrollConfig{
			ServerURL: *serverURL, TokenFile: *tokenFile,
			CAFile: *caFile, Version: buildinfo.Version,
		})
		if err != nil {
			return err
		}
		fmt.Printf("Agent %s enrolled for Host %s\n", identity.AgentID, identity.HostID)
		return nil
	}
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	state, err := agent.OpenState(*stateDirectory)
	if err != nil {
		return err
	}
	defer state.Close()
	if _, err := agent.LoadIdentity(state); err != nil {
		return errors.New("agent is not enrolled; run restfleet-agent enroll first")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	return agent.Run(ctx, state, agent.RunConfig{Version: buildinfo.Version})
}
