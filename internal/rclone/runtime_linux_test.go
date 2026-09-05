package rclone

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func tmpfsRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/dev/shm", "restfleet-runtime-test-")
	if err != nil {
		t.Fatal("tmpfs required for runtime security tests:", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func fakeRclone(t *testing.T, mode string) string {
	t.Helper()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	filename := filepath.Join(t.TempDir(), "rclone")
	quoted := "'" + strings.ReplaceAll(binary, "'", "'\\''") + "'"
	script := "#!/bin/sh\nexec " + quoted + " -test.run=^TestRuntimeChild$ -- " + mode + " \"$@\"\n"
	if err := os.WriteFile(filename, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return filename
}

// TestRuntimeChild runs only in a subprocess launched by the fake executable.
func TestRuntimeChild(t *testing.T) {
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
	args := os.Args[marker+1:]
	fail := func() { os.Exit(43) }
	if len(args) != 14 || args[1] != "lsjson" || args[2] != "encrypted:" ||
		args[3] != "--stat" || args[4] != "--config" ||
		strings.Join(args[6:], " ") != "--retries 1 --low-level-retries 1 --contimeout 10s --timeout 30s" {
		fail()
	}
	for _, variable := range os.Environ() {
		if strings.Contains(variable, "canary") || strings.HasPrefix(variable, "RCLONE_") ||
			strings.HasPrefix(variable, "HTTP_PROXY=") || strings.HasPrefix(variable, "HTTPS_PROXY=") {
			fail()
		}
	}
	mode, filename := args[0], args[5]
	info, err := os.Stat(filename)
	if err != nil || !privateFile(info) {
		fail()
	}
	dirInfo, err := os.Stat(filepath.Dir(filename))
	if err != nil || dirInfo.Mode().Perm() != 0700 {
		fail()
	}
	raw, err := os.ReadFile(filename)
	if err != nil {
		fail()
	}
	if mode == "refresh" || mode == "refresh-fail" || mode == "persist-fail" {
		for n := 1; n <= 2; n++ {
			next := bytes.ReplaceAll(raw, []byte("canary-refresh"), []byte(fmt.Sprintf("refreshed-%d", n)))
			tmp := filename + ".new"
			if os.WriteFile(tmp, next, 0600) != nil || os.Rename(tmp, filename) != nil {
				fail()
			}
			// Requiring a durable callback ACK proves synchronization happens while
			// the child is alive, not just in a graceful-shutdown final sync.
			ack := filename + ".ack" + strconv.Itoa(n)
			for deadline := time.Now().Add(5 * time.Second); ; {
				if _, err := os.Stat(ack); err == nil {
					break
				}
				if time.Now().After(deadline) {
					fail()
				}
				time.Sleep(10 * time.Millisecond)
			}
		}
	}
	switch mode {
	case "rename-gap":
		if os.Rename(filename, filename+".old") != nil {
			fail()
		}
		time.Sleep(500 * time.Millisecond)
		if os.WriteFile(filename, raw, 0600) != nil {
			fail()
		}
		_, _ = fmt.Fprint(os.Stdout, `{"IsDir":true}`)
	case "fail", "refresh-fail":
		_, _ = fmt.Fprintln(os.Stderr, "canary-access canary-refresh https://user:password@example.invalid")
		os.Exit(91)
	case "malformed":
		_, _ = fmt.Fprint(os.Stdout, "canary-access")
	case "missing":
		_, _ = fmt.Fprint(os.Stdout, "{}")
	case "not-dir":
		_, _ = fmt.Fprint(os.Stdout, `{"IsDir":false}`)
	case "overflow":
		_, _ = io.WriteString(os.Stdout, strings.Repeat("x", maxOutputBytes+1))
	case "change-target":
		next := bytes.ReplaceAll(raw, []byte("cloud:backups"), []byte("cloud:elsewhere"))
		if os.WriteFile(filename, next, 0600) != nil {
			fail()
		}
		_, _ = fmt.Fprint(os.Stdout, `{"IsDir":true}`)
	case "sleep":
		child := exec.Command("/bin/sleep", "30")
		if child.Start() != nil {
			fail()
		}
		if os.WriteFile(filename+".pid", []byte(strconv.Itoa(child.Process.Pid)), 0600) != nil {
			fail()
		}
		_ = child.Wait()
	default:
		_, _ = fmt.Fprint(os.Stdout, `{"IsDir":true,"FutureField":"ignored"}`)
	}
	os.Exit(0)
}

func TestRuntimeReadOnlyResultsAndRedaction(t *testing.T) {
	t.Setenv("RCLONE_CONFIG_ENCRYPTED_REMOTE", "canary-override")
	t.Setenv("HTTPS_PROXY", "https://canary-user:canary-secret@example.invalid")
	t.Setenv("RESTFLEET_MASTER_KEY", "canary-master")
	for _, tc := range []struct {
		mode string
		want error
	}{
		{"success", nil}, {"rename-gap", nil}, {"fail", ErrTestFailed}, {"malformed", ErrTestOutput},
		{"missing", ErrTestOutput}, {"not-dir", ErrTestOutput}, {"overflow", ErrTestOutput},
		{"change-target", ErrConfigChanged},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			root := tmpfsRoot(t)
			r, err := NewRuntime(root, fakeRclone(t, tc.mode))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = r.Close() }()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			err = r.Test(ctx, []byte(testConfig()), "encrypted", func(context.Context, []byte) error {
				t.Error("unexpected refresh")
				return nil
			})
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
			entries, _ := os.ReadDir(root)
			if len(entries) != 1 || entries[0].Name() != ".lock" {
				t.Fatal("plaintext not removed")
			}
		})
	}
}

func TestRuntimeRefreshPersistsWhileRunning(t *testing.T) {
	for _, mode := range []string{"refresh", "refresh-fail", "persist-fail"} {
		t.Run(mode, func(t *testing.T) {
			root := tmpfsRoot(t)
			r, err := NewRuntime(root, fakeRclone(t, mode))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = r.Close() }()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			calls := 0
			var borrowed []byte
			err = r.Test(ctx, []byte(testConfig()), "encrypted", func(_ context.Context, raw []byte) error {
				calls++
				borrowed = raw
				if !bytes.Contains(raw, []byte(fmt.Sprintf("refreshed-%d", calls))) {
					t.Error("wrong refresh")
				}
				if mode == "persist-fail" {
					return errors.New("canary-secret: database failure")
				}
				matches, _ := filepath.Glob(filepath.Join(root, "test-*", "rclone.conf"))
				if len(matches) != 1 {
					return errors.New("runtime config missing")
				}
				return os.WriteFile(matches[0]+".ack"+strconv.Itoa(calls), nil, 0600)
			})
			want := error(nil)
			expectedCalls := 2
			if mode == "refresh-fail" {
				want = ErrTestFailed
			}
			if mode == "persist-fail" {
				want = ErrRefreshPersist
				expectedCalls = 1
			}
			if !errors.Is(err, want) || calls != expectedCalls {
				t.Fatalf("err=%v calls=%d", err, calls)
			}
			if len(bytes.Trim(borrowed, "\x00")) != 0 {
				t.Fatal("borrowed plaintext not cleared")
			}
		})
	}
}

func TestRuntimeRejectsUnsafeFiles(t *testing.T) {
	root := tmpfsRoot(t)
	target := filepath.Join(root, "config")
	write := func() {
		t.Helper()
		if err := os.WriteFile(target, []byte(testConfig()), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write()
	if _, err := readRuntimeConfig(target, "encrypted"); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(target, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := readRuntimeConfig(target, "encrypted"); !errors.Is(err, ErrUnsafeRuntime) {
		t.Fatal(err)
	}
	if err := os.Chmod(target, 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := readRuntimeConfig(link, "encrypted"); !errors.Is(err, ErrUnsafeRuntime) {
		t.Fatal(err)
	}
	hard := filepath.Join(root, "hard")
	if err := os.Link(target, hard); err != nil {
		t.Fatal(err)
	}
	if _, err := readRuntimeConfig(target, "encrypted"); !errors.Is(err, ErrUnsafeRuntime) {
		t.Fatal(err)
	}
	if err := os.Remove(hard); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(root, "fifo")
	if err := syscall.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readRuntimeConfig(fifo, "encrypted"); !errors.Is(err, ErrUnsafeRuntime) {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, bytes.Repeat([]byte("x"), MaxConfigBytes+1), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readRuntimeConfig(target, "encrypted"); !errors.Is(err, ErrUnsafeRuntime) {
		t.Fatal(err)
	}
}

func TestRuntimeOwnershipAndRecovery(t *testing.T) {
	binary := fakeRclone(t, "success")
	root := tmpfsRoot(t)
	// The first owner must clean only stale runtime entries, never follow links.
	stale := filepath.Join(root, "test-123")
	if err := os.Mkdir(stale, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stale, "rclone.conf"), []byte("canary"), 0600); err != nil {
		t.Fatal(err)
	}
	unrelated := filepath.Join(root, "unrelated")
	if err := os.Mkdir(unrelated, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(unrelated, filepath.Join(root, "test-456")); err != nil {
		t.Fatal(err)
	}
	r, err := NewRuntime(root, binary)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("stale plaintext survived")
	}
	if _, err := os.Stat(unrelated); err != nil {
		t.Fatal("cleanup followed symlink")
	}
	if _, err := NewRuntime(root, binary); !errors.Is(err, ErrRuntimeBusy) {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if err := r.Test(context.Background(), []byte(testConfig()), "encrypted", func(context.Context, []byte) error { return nil }); !errors.Is(err, ErrRuntimeClosed) {
		t.Fatal(err)
	}
	r, err = NewRuntime(root, binary)
	if err != nil {
		t.Fatal(err)
	}
	_ = r.Close()
	if err := os.Chmod(root, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := NewRuntime(root, binary); !errors.Is(err, ErrUnsafeRuntime) {
		t.Fatal(err)
	}
	_ = os.Chmod(root, 0700)
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	if _, err := NewRuntime(alias, binary); !errors.Is(err, ErrUnsafeRuntime) {
		t.Fatal(err)
	}
	var fs syscall.Statfs_t
	disk := t.TempDir()
	if syscall.Statfs(disk, &fs) == nil && fs.Type != 0x01021994 {
		if _, err := NewRuntime(disk, binary); !errors.Is(err, ErrUnsafeRuntime) {
			t.Fatal(err)
		}
	}
}

func TestRuntimeCancellationKillsProcessGroup(t *testing.T) {
	root := tmpfsRoot(t)
	r, err := NewRuntime(root, fakeRclone(t, "sleep"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- r.Test(ctx, []byte(testConfig()), "encrypted", func(context.Context, []byte) error { return nil })
	}()
	pid := 0
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		matches, _ := filepath.Glob(filepath.Join(root, "test-*", "rclone.conf.pid"))
		if len(matches) == 1 {
			raw, _ := os.ReadFile(matches[0])
			pid, _ = strconv.Atoi(string(raw))
			if pid > 0 {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if pid == 0 {
		t.Fatal("child did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("process group cancellation hung")
	}
	// Orphan descendants may briefly be zombies until PID 1 reaps them.
	for deadline := time.Now().Add(time.Second); ; {
		raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if os.IsNotExist(err) || bytes.Contains(raw, []byte(") Z ")) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("descendant still running")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestRuntimeRefreshCannotChangeClientIdentity(t *testing.T) {
	base, err := ParseConfig(testConfig(), "encrypted")
	if err != nil {
		t.Fatal(err)
	}
	next, err := ParseConfig(strings.Replace(testConfig(), "type = onedrive", "type = onedrive\nclient_id = different-client", 1), "encrypted")
	if err != nil {
		t.Fatal(err)
	}
	if !base.SameTarget(next) || base.sameExceptToken(next) {
		t.Fatal("watcher must be stricter than administrator replacement")
	}
	if base.SameTarget(nil) || (*Config)(nil).SameTarget(base) {
		t.Fatal("nil config accepted")
	}
}
