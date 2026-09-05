package rclone

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sync"
	"syscall"
	"time"
)

var (
	ErrUnsafeRuntime  = errors.New("unsafe credential runtime")
	ErrRuntimeBusy    = errors.New("credential runtime is already owned")
	ErrRuntimeClosed  = errors.New("credential runtime is closed")
	ErrConfigChanged  = errors.New("credential runtime configuration changed unexpectedly")
	ErrRefreshPersist = errors.New("credential refresh could not be persisted")
	ErrTestFailed     = errors.New("storage connection test failed")
	ErrTestOutput     = errors.New("invalid storage connection test result")
)

const testTimeout = time.Minute
const watchInterval = 250 * time.Millisecond
const maxOutputBytes = 64 << 10

var runtimeName = regexp.MustCompile(`^test-[0-9]+$`)
var errConfigReplacing = errors.New("credential config replacement in progress")

// Runtime is a central-only owner of a private tmpfs directory. Close waits for
// active tests; cancel their contexts before closing during server shutdown.
type Runtime struct {
	mu     sync.RWMutex
	root   string
	binary string
	lock   *os.File
}

// NewRuntime requires an existing, canonical 0700 tmpfs directory owned by this
// service user. The exclusive lock fences cleanup from any other live runtime.
func NewRuntime(root, binary string) (*Runtime, error) {
	if !filepath.IsAbs(root) || filepath.Clean(root) != root || root == "/" ||
		!filepath.IsAbs(binary) || filepath.Clean(binary) != binary {
		return nil, ErrUnsafeRuntime
	}
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil || resolved != root {
		return nil, ErrUnsafeRuntime
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 || !owned(info) {
		return nil, ErrUnsafeRuntime
	}
	var fs syscall.Statfs_t
	if syscall.Statfs(root, &fs) != nil || fs.Type != 0x01021994 { // Linux TMPFS_MAGIC
		return nil, ErrUnsafeRuntime
	}
	executable, err := os.Stat(binary)
	if err != nil || !executable.Mode().IsRegular() || executable.Mode().Perm()&0111 == 0 ||
		executable.Mode().Perm()&0022 != 0 {
		return nil, ErrUnsafeRuntime
	}
	lock, err := os.OpenFile(filepath.Join(root, ".lock"),
		os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return nil, ErrUnsafeRuntime
	}
	valid, err := lock.Stat()
	if err != nil || !privateFile(valid) {
		_ = lock.Close()
		return nil, ErrUnsafeRuntime
	}
	if syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		_ = lock.Close()
		return nil, ErrRuntimeBusy
	}
	r := &Runtime{root: root, binary: binary, lock: lock}
	entries, err := os.ReadDir(root)
	if err == nil {
		for _, entry := range entries {
			if runtimeName.MatchString(entry.Name()) {
				// Only this runtime's tmpfs entries; RemoveAll does not follow symlinks.
				if err = os.RemoveAll(filepath.Join(root, entry.Name())); err != nil {
					break
				}
			}
		}
	}
	if err != nil {
		_ = r.Close()
		return nil, ErrUnsafeRuntime
	}
	return r, nil
}

func owned(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid())
}

func privateFile(info os.FileInfo) bool {
	if info == nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || !owned(info) {
		return false
	}
	stat := info.Sys().(*syscall.Stat_t)
	return stat.Nlink == 1
}

func (r *Runtime) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.lock == nil {
		return nil
	}
	err := r.lock.Close() // Closing the descriptor releases flock without unlinking it.
	r.lock = nil
	if err != nil {
		return ErrUnsafeRuntime
	}
	return nil
}

// Test only stats the crypt root: it neither creates nor modifies repository
// objects. persist MUST durably encrypt the new config using revision CAS and
// honor ctx. It receives borrowed plaintext, cleared immediately on return.
// Provider output, process errors and callback errors never cross this boundary.
func (r *Runtime) Test(ctx context.Context, raw []byte, remote string, persist func(context.Context, []byte) error) (resultErr error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.lock == nil {
		return ErrRuntimeClosed
	}
	if persist == nil {
		return ErrRefreshPersist
	}
	config, err := ParseConfig(string(raw), remote)
	if err != nil {
		return ErrInvalidConfig
	}
	ctx, cancel := context.WithTimeout(ctx, testTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return err
	}
	dir, err := os.MkdirTemp(r.root, "test-")
	if err != nil {
		return ErrUnsafeRuntime
	}
	// The whole directory is private: rclone also creates temporary rename files.
	defer func() {
		if os.RemoveAll(dir) != nil {
			resultErr = ErrUnsafeRuntime
		}
	}()
	filename := filepath.Join(dir, "rclone.conf")
	current := config.Bytes()
	defer func() { clear(current) }()
	file, err := os.OpenFile(filename, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return ErrUnsafeRuntime
	}
	_, writeErr := file.Write(current)
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		return ErrUnsafeRuntime
	}

	syncConfig := func() error {
		next, err := readUpdatedConfig(ctx, filename, remote)
		if err != nil {
			return err
		}
		if !config.SameExceptToken(next) {
			return ErrConfigChanged
		}
		encoded := next.Bytes()
		defer clear(encoded)
		if bytes.Equal(current, encoded) {
			return nil
		}
		if err := persist(ctx, encoded); err != nil {
			return ErrRefreshPersist
		}
		clear(current)
		current = bytes.Clone(encoded)
		return nil
	}

	cmd := exec.CommandContext(ctx, r.binary, "lsjson", remote+":", "--stat",
		"--config", filename, "--retries", "1", "--low-level-retries", "1",
		"--contimeout", "10s", "--timeout", "30s")
	cmd.Dir = dir
	// Do not inherit RCLONE_*, proxy variables, default config paths, or secrets.
	cmd.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin", "LANG=C", "TMPDIR=" + dir}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = 2 * time.Second
	output := &boundedOutput{}
	cmd.Stdout = output
	cmd.Stderr = io.Discard // Never retain raw provider errors (may contain OAuth secrets).
	defer func() { clear(output.data) }()
	if cmd.Start() != nil {
		return ErrTestFailed
	}
	defer func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }()
	finished := make(chan error, 1)
	go func() { finished <- cmd.Wait() }()
	ticker := time.NewTicker(watchInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			cancel()
			<-finished
			return ctx.Err()
		case <-ticker.C:
			if err := syncConfig(); err != nil {
				cancel()
				<-finished
				return err
			}
		case err := <-finished:
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// Persist a refresh even when the read-only test subsequently fails.
			if syncErr := syncConfig(); syncErr != nil {
				return syncErr
			}
			if err != nil {
				return ErrTestFailed
			}
			if output.overflow {
				return ErrTestOutput
			}
			var result struct{ IsDir *bool }
			if json.Unmarshal(output.data, &result) != nil || result.IsDir == nil || !*result.IsDir {
				return ErrTestOutput
			}
			return nil
		}
	}
}

// rclone first renames the old config to a backup, then moves the new file
// into place. Tolerate only that short ENOENT gap; every unsafe inode fails closed.
func readUpdatedConfig(ctx context.Context, filename, remote string) (*Config, error) {
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	retry := time.NewTicker(10 * time.Millisecond)
	defer retry.Stop()
	for {
		config, err := readRuntimeConfig(filename, remote)
		if !errors.Is(err, errConfigReplacing) {
			return config, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline.C:
			return nil, ErrUnsafeRuntime
		case <-retry.C:
		}
	}
}

// O_NONBLOCK avoids hanging on a substituted FIFO. Check the opened inode,
// not just its pathname, and reject symlinks, hard links and permissive modes.
func readRuntimeConfig(filename, remote string) (*Config, error) {
	f, err := os.OpenFile(filename, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if os.IsNotExist(err) {
		return nil, errConfigReplacing
	}
	if err != nil {
		return nil, ErrUnsafeRuntime
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil || !privateFile(info) || info.Size() > MaxConfigBytes {
		return nil, ErrUnsafeRuntime
	}
	raw, err := io.ReadAll(io.LimitReader(f, MaxConfigBytes+1))
	defer clear(raw)
	if err != nil {
		return nil, ErrUnsafeRuntime
	}
	config, err := ParseConfig(string(raw), remote)
	if err != nil {
		return nil, ErrConfigChanged
	}
	return config, nil
}

type boundedOutput struct {
	data     []byte
	overflow bool
}

func (b *boundedOutput) Write(p []byte) (int, error) {
	n := len(p)
	remaining := maxOutputBytes - len(b.data)
	if len(p) > remaining {
		b.overflow = true
		p = p[:remaining]
	}
	b.data = append(b.data, p...)
	return n, nil // Drain the pipe without retaining unbounded output.
}
