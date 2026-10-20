package main

import (
	"encoding/json"
	"flag"
	"io"
	"os"

	"github.com/sagehou/restfleet/internal/gateway"
)

func runProvision(arguments []string, output io.Writer) error {
	flags := flag.NewFlagSet("provision", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	configPath := flags.String("config-file", "", "protected local runtime provisioning metadata")
	statePath := flags.String("state-file", "", "durable protected provisioning state")
	if flags.Parse(arguments) != nil || flags.NArg() != 0 || *configPath == "" || *statePath == "" {
		return gateway.ErrProvision
	}
	config, err := gateway.LoadProvisionConfig(*configPath)
	if err != nil || (config.Environment == "production" && os.Geteuid() == 0) {
		return gateway.ErrProvision
	}
	result, err := gateway.Provision(config, *statePath)
	if err != nil {
		return gateway.ErrProvision
	}
	if json.NewEncoder(output).Encode(result) != nil {
		return gateway.ErrProvision
	}
	return nil
}

func runFinalize(arguments []string) error {
	flags := flag.NewFlagSet("finalize", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	configPath := flags.String("config-file", "", "protected local runtime provisioning metadata")
	statePath := flags.String("state-file", "", "durable protected provisioning state")
	planPath := flags.String("plan-file", "", "central prepared startup plan, copied to the Gateway identity")
	registrationPath := flags.String("registration-file", "", "central audit registration result, copied to the Gateway identity")
	outputPath := flags.String("output-file", "", "protected service.json to create")
	if flags.Parse(arguments) != nil || flags.NArg() != 0 || *configPath == "" || *statePath == "" ||
		*planPath == "" || *registrationPath == "" || *outputPath == "" {
		return gateway.ErrFinalize
	}
	config, err := gateway.LoadProvisionConfig(*configPath)
	if err != nil || (config.Environment == "production" && os.Geteuid() == 0) {
		return gateway.ErrFinalize
	}
	return gateway.Finalize(config, *statePath, *planPath, *registrationPath, *outputPath)
}
