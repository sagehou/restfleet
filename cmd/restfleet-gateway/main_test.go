package main

import (
	"bytes"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sagehou/restfleet/internal/security"
)

func TestSourceCommandsPublishOnlyPublicIdentityAndNeverOverwrite(t *testing.T) {
	dir := t.TempDir()
	if os.Chmod(dir, 0700) != nil {
		t.Fatal("private source directory")
	}
	path := filepath.Join(dir, "source")
	var initialized, loaded bytes.Buffer
	if run([]string{"source-init", "--source-key-file", path}, &initialized) != nil ||
		run([]string{"source-public", "--source-key-file", path}, &loaded) != nil || initialized.String() != loaded.String() {
		t.Fatal("source command round trip")
	}
	public, err := base64.StdEncoding.DecodeString(strings.TrimSpace(initialized.String()))
	seed, readErr := security.ReadProtectedKey(path, 32)
	defer clear(seed)
	if err != nil || readErr != nil || len(public) != 32 || bytes.Equal(public, seed) {
		t.Fatal("source command leaked private seed")
	}
	var duplicate bytes.Buffer
	if run([]string{"source-init", "--source-key-file", path}, &duplicate) != security.ErrGatewaySource || duplicate.Len() != 0 {
		t.Fatal("duplicate initialization replaced identity or published a key")
	}
	loaded.Reset()
	if run([]string{"source-public", "--source-key-file", path}, &loaded) != nil || loaded.String() != initialized.String() {
		t.Fatal("duplicate initialization changed identity")
	}
}

type failingOutput struct{}

func (failingOutput) Write([]byte) (int, error) { return 0, errors.New("private-output-canary") }

func TestGatewayCommandRejectsArgumentsAndRedactsErrors(t *testing.T) {
	for _, arguments := range [][]string{
		{"private-command-canary"},
		{"source-init"},
		{"source-public", "--private-flag-canary=private-value-canary"},
		{"source-init", "--source-key-file", "private-path-canary"},
		{"source-init", "--source-key-file", "/private-path-canary", "private-extra-canary"},
	} {
		var output bytes.Buffer
		err := run(arguments, &output)
		if err == nil || strings.Contains(err.Error(), "canary") || output.Len() != 0 {
			t.Fatal("command rejection echoed untrusted arguments")
		}
	}
	if err := run([]string{"version"}, failingOutput{}); err != security.ErrGatewaySource {
		t.Fatal("output error leaked")
	}
	var output bytes.Buffer
	if run(nil, &output) != nil || !strings.HasPrefix(output.String(), "restfleet-gateway ") {
		t.Fatal("default version behavior")
	}
}
