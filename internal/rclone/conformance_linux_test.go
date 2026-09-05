package rclone

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// CI supplies the same digest-pinned official binary that central images use.
// This smoke test is deliberately offline: it verifies CLI/config/JSON contracts,
// not real OneDrive authorization or write permissions.
func TestPinnedRcloneConformance(t *testing.T) {
	binary := os.Getenv("RESTFLEET_TEST_RCLONE_BINARY")
	if binary == "" {
		t.Skip("CI supplies the pinned rclone executable")
	}
	root := tmpfsRoot(t)
	config, err := ParseConfig(testConfig(), "encrypted")
	if err != nil {
		t.Fatal(err)
	}
	filename := filepath.Join(root, "rclone.conf")
	raw := config.Bytes()
	defer clear(raw)
	if err := os.WriteFile(filename, raw, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	run := func(args ...string) []byte {
		t.Helper()
		cmd := exec.CommandContext(ctx, binary, args...)
		cmd.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin", "LANG=C", "TMPDIR=" + root}
		cmd.Dir = root
		out, err := cmd.Output()
		if err != nil {
			t.Fatal("pinned rclone conformance command failed")
		}
		return out
	}
	if !bytes.HasPrefix(run("version"), []byte("rclone v1.75.1\n")) {
		t.Fatal("unapproved rclone version")
	}
	names := strings.Fields(string(run("listremotes", "--config", filename)))
	if len(names) != 2 || names[0] != "cloud:" || names[1] != "encrypted:" {
		t.Fatal("canonical config rejected")
	}
	out := run("lsjson", root, "--stat", "--config", filename, "--retries", "1", "--low-level-retries", "1", "--contimeout", "10s", "--timeout", "30s")
	var stat struct{ IsDir bool }
	if json.Unmarshal(out, &stat) != nil || !stat.IsDir {
		t.Fatal("lsjson stat contract changed")
	}
}
