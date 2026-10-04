package main

import (
	"testing"

	control "github.com/sagehou/restfleet/internal/server"
)

func TestGatewayStartupCommandRejectsUnsafeInputBeforeOpeningDatabase(t *testing.T) {
	for _, args := range [][]string{
		nil, {"version"}, {"gateway-start"}, {"gateway-start", "--help"},
		{"gateway-start", "--config-file", "private-path-canary"},
		{"gateway-start", "--config-file", "/missing/private-path-canary", "unexpected"},
		{"gateway-start", "--secret", "private-secret-canary"},
	} {
		if err := runGatewayStartup(args); err != control.ErrGatewayStartup {
			t.Fatal("unsafe command accepted or raw diagnostic returned")
		}
	}
}
