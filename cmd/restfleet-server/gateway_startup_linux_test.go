package main

import (
	"bytes"
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
		var output bytes.Buffer
		if err := runGatewayCommand(args, &output); err != control.ErrGatewayStartup || output.Len() != 0 {
			t.Fatal("unsafe command accepted or raw diagnostic returned")
		}
	}
}

func TestGatewayAuditCommandRejectsUnsafeInputWithoutOutput(t *testing.T) {
	for _, args := range [][]string{
		{"gateway-audit-register"}, {"gateway-audit-register", "--help"},
		{"gateway-audit-register", "--config-file", "private-path-canary"},
		{"gateway-audit-register", "--config-file", "/missing/private-path-canary", "unexpected"},
		{"gateway-audit-register", "--secret", "private-secret-canary"},
	} {
		var output bytes.Buffer
		if err := runGatewayCommand(args, &output); err != control.ErrGatewayAuditRegistration || output.Len() != 0 {
			t.Fatal("unsafe registration command accepted or raw diagnostic returned")
		}
	}
	if err := runGatewayCommand([]string{"gateway-audit-register", "--config-file", "private-path-canary"}, nil); err != control.ErrGatewayAuditRegistration {
		t.Fatal("missing output accepted")
	}
}
