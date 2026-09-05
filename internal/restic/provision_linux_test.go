package restic

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sagehou/restfleet/internal/rclone"
)

const fixtureID = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func requestFixture() ProvisionRequest {
	return ProvisionRequest{
		GatewayID:    uuid.MustParse("019abcde-1234-7000-8000-000000000001"),
		RepositoryID: uuid.MustParse("019abcde-1234-7000-8000-000000000002"),
		Remote:       "encrypted",
		Password:     []byte("test-only-repository-password-32-bytes"),
		Config: []byte("[cloud]\ntype = onedrive\ndrive_id = fixture-drive\ndrive_type = personal\n" +
			"token = {\"access_token\":\"test-access\",\"token_type\":\"Bearer\",\"refresh_token\":\"test-refresh\",\"expiry\":\"2026-12-01T00:00:00Z\"}\n" +
			"[encrypted]\ntype = crypt\nremote = cloud:backups\npassword = AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA\n"),
	}
}

func privateRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/dev/shm", "rf-init-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func fakeExecutable(t *testing.T, mode, state string) string {
	t.Helper()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
	filename := filepath.Join(t.TempDir(), "engine")
	script := "#!/bin/sh\nexec " + quote(binary) + " -test.run=^TestProvisionChild$ -- " +
		quote(mode) + " " + quote(state) + " \"$@\"\n"
	if err := os.WriteFile(filename, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return filename
}

func setupProvisioner(t *testing.T, engineMode, backendMode string) (*Provisioner, string, string) {
	t.Helper()
	state, root := t.TempDir(), privateRoot(t)
	runtime, err := rclone.NewRuntime(root, fakeExecutable(t, backendMode, state))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	p, err := NewProvisioner(fakeExecutable(t, engineMode, state), runtime)
	if err != nil {
		t.Fatal(err)
	}
	return p, state, root
}

func noRefresh(context.Context, []byte) error { return nil }

// TestProvisionChild is a fake executable; shell is confined to the test shim.
func TestProvisionChild(t *testing.T) {
	marker := -1
	for i, arg := range os.Args {
		if arg == "--" {
			marker = i
			break
		}
	}
	if marker < 0 {
		return
	}
	mode, state, args := os.Args[marker+1], os.Args[marker+2], os.Args[marker+3:]
	fail := func() { os.Exit(43) }
	write := func(name, value string) {
		if os.WriteFile(filepath.Join(state, name), []byte(value), 0600) != nil {
			fail()
		}
	}
	for _, env := range os.Environ() {
		if strings.Contains(env, "parent-canary") || strings.HasPrefix(env, "RCLONE_") ||
			strings.HasPrefix(env, "HTTP_PROXY=") || strings.HasPrefix(env, "HTTPS_PROXY=") {
			fail()
		}
	}
	spawn := func(name string) *exec.Cmd {
		child := exec.Command("/bin/sleep", "30")
		if child.Start() != nil {
			fail()
		}
		write(name, strconv.Itoa(child.Process.Pid))
		return child
	}
	if strings.Contains(mode, "backend") {
		fixture := requestFixture()
		want := []string{"serve", "restic", "encrypted:restfleet/agents/" + fixture.GatewayID.String() + "/" + fixture.RepositoryID.String(),
			"--config", "", "--addr", "", "--cache-objects=false", "--retries", "1",
			"--low-level-retries", "1", "--contimeout", "10s", "--timeout", "30s"}
		if len(args) != len(want) {
			fail()
		}
		want[4], want[6] = args[4], args[6]
		if !reflect.DeepEqual(args, want) || filepath.Dir(args[4]) != filepath.Dir(args[6]) {
			fail()
		}
		if mode == "real-backend" {
			binary, err := os.ReadFile(filepath.Join(state, "real-rclone"))
			if err != nil {
				fail()
			}
			// Test-only local backend substitution. The production parser still
			// accepts only OneDrive+crypt and never sees this override.
			args[2] = filepath.Join(state, "repo")
			if syscall.Exec(string(binary), append([]string{string(binary)}, args...), os.Environ()) != nil {
				fail()
			}
		}
		if mode == "backend-fail" {
			_, _ = fmt.Fprint(os.Stderr, "test-access test-refresh provider-secret")
			os.Exit(90)
		}
		write("backend-pid", strconv.Itoa(os.Getpid()))
		if mode == "backend-refresh" {
			raw, err := os.ReadFile(args[4])
			if err != nil {
				fail()
			}
			next := bytes.ReplaceAll(raw, []byte("test-refresh"), []byte("refreshed-token"))
			if os.WriteFile(args[4]+".new", next, 0600) != nil || os.Rename(args[4]+".new", args[4]) != nil {
				fail()
			}
			for deadline := time.Now().Add(5 * time.Second); ; {
				if _, err := os.Stat(filepath.Join(state, "refresh-ack")); err == nil {
					break
				}
				if time.Now().After(deadline) {
					fail()
				}
				time.Sleep(10 * time.Millisecond)
			}
		}
		listener, err := net.Listen("unix", args[6])
		if err != nil {
			fail()
		}
		if mode == "backend-cancel" {
			_ = spawn("backend-descendant")
		}
		if mode == "backend-dies" {
			for {
				if _, err := os.Stat(filepath.Join(state, "engine-pid")); err == nil {
					os.Exit(91)
				}
				time.Sleep(10 * time.Millisecond)
			}
		}
		for {
			conn, err := listener.Accept()
			if err != nil {
				fail()
			}
			_ = conn.Close()
		}
	}
	if len(args) < 4 || args[0] != "--json" || args[1] != "--cache-dir" {
		fail()
	}
	raw, err := os.ReadFile(os.Getenv("RESTIC_PASSWORD_FILE"))
	if err != nil || !bytes.Equal(raw, requestFixture().Password) {
		fail()
	}
	for _, filename := range []string{os.Getenv("RESTIC_PASSWORD_FILE"), args[2]} {
		info, err := os.Stat(filename)
		wantMode := os.FileMode(0600)
		if filename == args[2] {
			wantMode = 0700
		}
		if err != nil || info.Mode().Perm() != wantMode {
			fail()
		}
	}
	u, err := url.Parse(strings.TrimPrefix(os.Getenv("RESTIC_REPOSITORY"), "rest:"))
	if err != nil || u.Scheme != "http+unix" || u.Host != "" || u.User != nil ||
		u.RawQuery != "" || u.Fragment != "" || !strings.HasSuffix(u.Path, "/rest.sock:/") {
		fail()
	}
	verb := strings.Join(args[3:], " ")
	if verb != "cat config" && verb != "init --repository-version 2" && verb != "snapshots" {
		fail()
	}
	calls, err := os.OpenFile(filepath.Join(state, "calls"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		fail()
	}
	_, _ = fmt.Fprintln(calls, verb)
	_ = calls.Close()
	write("engine-pid", strconv.Itoa(os.Getpid()))
	if mode == "cancel" {
		child := spawn("engine-descendant")
		_ = child.Wait()
		os.Exit(0)
	}
	if strings.HasPrefix(mode, "exit-") {
		code, _ := strconv.Atoi(strings.TrimPrefix(mode, "exit-"))
		_, _ = fmt.Fprint(os.Stderr, "test-access test-refresh provider-secret")
		_, _ = fmt.Fprint(os.Stdout, "test-only-repository-password-32-bytes")
		os.Exit(code)
	}
	id := fixtureID
	switch verb {
	case "cat config":
		if _, err := os.Stat(filepath.Join(state, "initialized")); os.IsNotExist(err) {
			os.Exit(10)
		}
		if mode == "malformed" {
			_, _ = fmt.Fprint(os.Stdout, "provider-secret")
		} else if mode == "missing-field" {
			_, _ = fmt.Fprint(os.Stdout, `{"version":2}`)
		} else if mode == "overflow" {
			_, _ = fmt.Fprint(os.Stdout, strings.Repeat("x", maxCommandOutput+1))
		} else {
			version := 2
			if mode == "format-1" {
				version = 1
			}
			if mode == "mismatch" {
				id = strings.Repeat("f", 64)
			}
			_, _ = fmt.Fprintf(os.Stdout, `{"id":%q,"version":%d,"future":true}`, id, version)
		}
	case "init --repository-version 2":
		if _, err := os.Stat(filepath.Join(state, "initialized")); err == nil {
			fail() // Reinitializing an existing repository is forbidden.
		}
		write("initialized", id)
		if mode == "init-malformed" {
			_, _ = fmt.Fprint(os.Stdout, `{"message_type":"initialized"}`)
		} else if mode == "init-failed" {
			os.Exit(1) // Ambiguous result after remote creation.
		} else {
			_, _ = fmt.Fprintf(os.Stdout, `{"message_type":"future","value":1}
{"message_type":"initialized","id":%q,"future":true}
`, id)
		}
	case "snapshots":
		switch mode {
		case "snapshots-null":
			_, _ = fmt.Fprint(os.Stdout, "null")
		case "snapshots-malformed":
			_, _ = fmt.Fprint(os.Stdout, "{}")
		case "snapshots-partial":
			_, _ = fmt.Fprint(os.Stdout, "[]")
			os.Exit(3)
		case "snapshots-exist":
			_, _ = fmt.Fprint(os.Stdout, "[{}]")
		default:
			_, _ = fmt.Fprint(os.Stdout, "[]")
		}
	}
	os.Exit(0)
}

func TestProvisionInitializesAndResumesWithoutAnotherWrite(t *testing.T) {
	t.Setenv("RESTIC_PASSWORD", "parent-canary")
	t.Setenv("RCLONE_CONFIG", "parent-canary")
	t.Setenv("HTTPS_PROXY", "parent-canary")
	p, state, root := setupProvisioner(t, "success", "backend")
	request := requestFixture()
	first, err := p.Provision(context.Background(), request, noRefresh)
	if err != nil || first.ID != fixtureID || first.FormatVersion != 2 {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	request.ExpectedID = first.ID
	second, err := p.Provision(context.Background(), request, noRefresh)
	if err != nil || first != second {
		t.Fatalf("resume=%+v err=%v", second, err)
	}
	calls, _ := os.ReadFile(filepath.Join(state, "calls"))
	if string(calls) != "cat config\ninit --repository-version 2\ncat config\nsnapshots\ncat config\nsnapshots\n" {
		t.Fatalf("unexpected commands: %s", calls)
	}
	entries, _ := os.ReadDir(root)
	if len(entries) != 1 || entries[0].Name() != ".lock" {
		t.Fatal("plaintext runtime survived")
	}
}

func TestProvisionRejectsFailuresWithoutSuccessMetadata(t *testing.T) {
	for _, tc := range []struct {
		mode string
		want error
	}{
		{"exit-1", ErrProcessFailed}, {"exit-3", ErrProcessFailed}, {"exit-11", ErrRepositoryLocked},
		{"exit-12", ErrWrongPassword}, {"exit-91", ErrProcessFailed},
		{"malformed", ErrInvalidOutput}, {"missing-field", ErrInvalidOutput}, {"overflow", ErrInvalidOutput},
		{"format-1", ErrRepositoryMatch}, {"mismatch", ErrRepositoryMatch},
		{"init-malformed", ErrInvalidOutput}, {"init-failed", ErrProcessFailed},
		{"snapshots-null", ErrInvalidOutput}, {"snapshots-malformed", ErrInvalidOutput},
		{"snapshots-partial", ErrProcessFailed}, {"snapshots-exist", ErrRepositoryInUse},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			p, state, root := setupProvisioner(t, tc.mode, "backend")
			info, err := p.Provision(context.Background(), requestFixture(), noRefresh)
			if !errors.Is(err, tc.want) || info != (RepositoryInfo{}) {
				t.Fatalf("info=%+v err=%v want=%v", info, err, tc.want)
			}
			// Initial read failures cannot trigger init.
			if strings.HasPrefix(tc.mode, "exit-") {
				calls, _ := os.ReadFile(filepath.Join(state, "calls"))
				if string(calls) != "cat config\n" {
					t.Fatal("read failure caused a write")
				}
			}
			entries, _ := os.ReadDir(root)
			if len(entries) != 1 {
				t.Fatal("failed command retained plaintext")
			}
		})
	}
}

func TestProvisionMissingRecordedRepositoryDoesNotReinitialize(t *testing.T) {
	p, state, _ := setupProvisioner(t, "success", "backend")
	request := requestFixture()
	request.ExpectedID = fixtureID
	_, err := p.Provision(context.Background(), request, noRefresh)
	calls, _ := os.ReadFile(filepath.Join(state, "calls"))
	if !errors.Is(err, ErrRepositoryAbsent) || string(calls) != "cat config\n" {
		t.Fatalf("err=%v calls=%s", err, calls)
	}
}

func TestProvisionRejectsInvalidRequestBeforeExecuting(t *testing.T) {
	p, state, _ := setupProvisioner(t, "success", "backend")
	for _, change := range []func(*ProvisionRequest){
		func(r *ProvisionRequest) { r.GatewayID = uuid.Nil },
		func(r *ProvisionRequest) { r.RepositoryID = uuid.New() },
		func(r *ProvisionRequest) { r.Remote = "encrypted:../../other" },
		func(r *ProvisionRequest) { r.Config = []byte("malicious configuration") },
		func(r *ProvisionRequest) { r.Password = nil },
		func(r *ProvisionRequest) { r.Password = append(r.Password, '\n') },
		func(r *ProvisionRequest) { r.ExpectedID = "short" },
	} {
		request := requestFixture()
		change(&request)
		if _, err := p.Provision(context.Background(), request, noRefresh); err == nil {
			t.Fatal("invalid provisioning request accepted")
		}
	}
	if _, err := os.Stat(filepath.Join(state, "backend-pid")); !os.IsNotExist(err) {
		t.Fatal("invalid request started backend")
	}
	if _, err := NewProvisioner("/missing/engine", p.runtime); !errors.Is(err, ErrInvalidProvision) {
		t.Fatal("invalid binary accepted")
	}
	if _, err := NewProvisioner(p.binary, nil); !errors.Is(err, ErrInvalidProvision) {
		t.Fatal("nil runtime accepted")
	}
}

func TestProvisionRefreshMustPersistWhileBackendIsRunning(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(strconv.FormatBool(fail), func(t *testing.T) {
			p, state, _ := setupProvisioner(t, "success", "backend-refresh")
			calls := 0
			var borrowed []byte
			_, err := p.Provision(context.Background(), requestFixture(), func(_ context.Context, raw []byte) error {
				calls++
				borrowed = raw
				if !bytes.Contains(raw, []byte("refreshed-token")) {
					t.Error("missing refreshed token")
				}
				if fail {
					return errors.New("provider-secret")
				}
				return os.WriteFile(filepath.Join(state, "refresh-ack"), nil, 0600)
			})
			if calls != 1 || (fail && !errors.Is(err, rclone.ErrRefreshPersist)) || (!fail && err != nil) {
				t.Fatalf("err=%v calls=%d", err, calls)
			}
			if len(bytes.Trim(borrowed, "\x00")) != 0 {
				t.Fatal("borrowed secret not cleared")
			}
		})
	}
}

func waitPID(t *testing.T, state, name string) int {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		raw, err := os.ReadFile(filepath.Join(state, name))
		pid, parseErr := strconv.Atoi(string(raw))
		if err == nil && parseErr == nil && pid > 0 {
			return pid
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s did not start", name)
	return 0
}

func assertStopped(t *testing.T, pid int) {
	t.Helper()
	for deadline := time.Now().Add(time.Second); ; {
		raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if os.IsNotExist(err) || bytes.Contains(raw, []byte(") Z ")) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("process %d survived cancellation", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestProvisionCancellationKillsBothProcessGroups(t *testing.T) {
	p, state, root := setupProvisioner(t, "cancel", "backend-cancel")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := p.Provision(ctx, requestFixture(), noRefresh)
		done <- err
	}()
	pids := []int{
		waitPID(t, state, "backend-pid"), waitPID(t, state, "backend-descendant"),
		waitPID(t, state, "engine-pid"), waitPID(t, state, "engine-descendant"),
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancellation hung")
	}
	for _, pid := range pids {
		assertStopped(t, pid)
	}
	entries, _ := os.ReadDir(root)
	if len(entries) != 1 {
		t.Fatal("cancellation retained plaintext")
	}
}

func TestProvisionBackendFailureCancelsEngine(t *testing.T) {
	for _, mode := range []string{"backend-fail", "backend-dies"} {
		t.Run(mode, func(t *testing.T) {
			p, _, _ := setupProvisioner(t, "cancel", mode)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if _, err := p.Provision(ctx, requestFixture(), noRefresh); !errors.Is(err, ErrBackendFailed) {
				t.Fatal(err)
			}
		})
	}
}

func TestInitializedJSONContract(t *testing.T) {
	for _, raw := range []string{
		"", "null", "{}", `{"message_type":"future"}`, `{"message_type":"initialized","id":"short"}`,
		`{"message_type":"initialized","id":"` + fixtureID + `"} trailing`,
		strings.Repeat(`{"message_type":"initialized","id":"`+fixtureID+`"}`, 2),
	} {
		if _, err := initializedID([]byte(raw)); !errors.Is(err, ErrInvalidOutput) {
			t.Fatalf("invalid initialization output accepted: %v", err)
		}
	}
	raw, _ := json.Marshal(map[string]string{"message_type": "initialized", "id": fixtureID})
	if id, err := initializedID(raw); err != nil || id != fixtureID {
		t.Fatalf("id=%s err=%v", id, err)
	}
}
