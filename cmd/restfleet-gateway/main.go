package main

import (
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/sagehou/restfleet/internal/buildinfo"
	"github.com/sagehou/restfleet/internal/security"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}
}

func run(arguments []string, output io.Writer) error {
	if len(arguments) == 0 || (len(arguments) == 1 && arguments[0] == "version") {
		_, err := fmt.Fprintf(output, "restfleet-gateway %s\n", buildinfo.String())
		if err != nil {
			return security.ErrGatewaySource
		}
		return nil
	}
	command := arguments[0]
	if command == "replay" {
		return runReplay(arguments[1:], output)
	}
	usage := errors.New("gateway requires source-init/source-public --source-key-file or replay --config-file")
	if command != "source-init" && command != "source-public" {
		return usage
	}
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	path := flags.String("source-key-file", "", "protected local Gateway source seed file")
	if flags.Parse(arguments[1:]) != nil || flags.NArg() != 0 || *path == "" {
		return usage
	}
	var public []byte
	if command == "source-init" {
		key, err := security.CreateGatewaySource(*path)
		if err != nil {
			return security.ErrGatewaySource
		}
		public = key
	} else {
		key, err := security.LoadGatewaySource(*path)
		if err != nil {
			return security.ErrGatewaySource
		}
		defer clear(key)
		public = key[32:]
	}
	if _, err := fmt.Fprintln(output, base64.StdEncoding.EncodeToString(public)); err != nil {
		return security.ErrGatewaySource
	}
	return nil
}
