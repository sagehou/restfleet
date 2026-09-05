package restic

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sagehou/restfleet/internal/rclone"
)

// CI supplies the same digest-pinned binaries that shipping images contain.
// Only the cloud backend is replaced with a local directory in the test shim:
// real Restic, real rclone, Unix HTTP, locks and repository format are exercised.
func TestPinnedRepositoryProvisioning(t *testing.T) {
	resticBinary := os.Getenv("RESTFLEET_TEST_RESTIC_BINARY")
	rcloneBinary := os.Getenv("RESTFLEET_TEST_RCLONE_BINARY")
	if resticBinary == "" || rcloneBinary == "" {
		t.Skip("CI supplies pinned Restic and rclone executables")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	version := exec.CommandContext(ctx, resticBinary, "version")
	version.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin", "LANG=C"}
	raw, err := version.Output()
	if err != nil || !strings.HasPrefix(string(raw), "restic 0.19.1 ") {
		t.Fatal("unapproved Restic binary")
	}
	state, root := t.TempDir(), privateRoot(t)
	if err := os.WriteFile(filepath.Join(state, "real-rclone"), []byte(rcloneBinary), 0600); err != nil {
		t.Fatal(err)
	}
	runtime, err := rclone.NewRuntime(root, fakeExecutable(t, "real-backend", state))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = runtime.Close() }()
	p, err := NewProvisioner(resticBinary, runtime)
	if err != nil {
		t.Fatal(err)
	}
	request := requestFixture()
	first, err := p.Provision(ctx, request, noRefresh)
	if err != nil || !repositoryIDPattern.MatchString(first.ID) || first.FormatVersion != 2 {
		t.Fatalf("pinned initialization failed: %v", err)
	}
	repo := filepath.Join(state, "repo")
	original, err := os.ReadFile(filepath.Join(repo, "config"))
	if err != nil {
		t.Fatal(err)
	}
	request.ExpectedID = first.ID
	second, err := p.Provision(ctx, request, noRefresh)
	if err != nil || second != first {
		t.Fatalf("pinned retry failed: %v", err)
	}
	request.Password = []byte("wrong-test-only-password-at-least-32-bytes")
	if _, err := p.Provision(ctx, request, noRefresh); !errors.Is(err, ErrWrongPassword) {
		t.Fatalf("wrong password accepted or misclassified: %v", err)
	}
	after, err := os.ReadFile(filepath.Join(repo, "config"))
	if err != nil || string(original) != string(after) {
		t.Fatal("retry or wrong password changed existing config")
	}
	keys, err := os.ReadDir(filepath.Join(repo, "keys"))
	if err != nil || len(keys) != 1 {
		t.Fatal("retry created another repository key")
	}
	locks, err := os.ReadDir(filepath.Join(repo, "locks"))
	if err != nil || len(locks) != 0 {
		t.Fatal("completed central reads left locks")
	}
	entries, _ := os.ReadDir(root)
	if len(entries) != 1 || entries[0].Name() != ".lock" {
		t.Fatal("plaintext or socket survived successful provisioning")
	}
}
