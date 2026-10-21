// Package restic builds argv-only Restic invocations, parses their JSONL
// progress stream and runs them in their own process group.
//
// The package never uses a shell, never puts a password on argv and never
// forwards raw Restic output to the network. Callers own transport, hooks and
// result persistence.
package restic

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"

	"github.com/google/uuid"
)

// BackupRequest is the effective, already-validated plan config for one run.
type BackupRequest struct {
	Repository      string
	PasswordFile    string
	HostID          uuid.UUID
	PlanID          uuid.UUID
	Paths           []string
	ExcludePatterns []string
	ExcludeCaches   bool
	OneFileSystem   bool
	SkipIfUnchanged bool
	// TLSClientCertFile holds the combined client certificate and key for the
	// Gateway REST endpoint; CACertFile pins the Gateway CA.
	TLSClientCertFile string
	CACertFile        string
}

// PreparedFiles holds the generated mode-0600 inputs for one invocation.
type PreparedFiles struct {
	PathsFile   string
	ExcludeFile string
	directory   string
}

// Prepare writes paths and excludes into a private directory. Restic reads them
// with --files-from-verbatim / --exclude-file so argv stays small and logs never
// contain the patterns.
func Prepare(paths, excludes []string) (PreparedFiles, error) {
	if len(paths) == 0 {
		return PreparedFiles{}, errors.New("restic: at least one path is required")
	}
	for _, path := range paths {
		if len(path) == 0 || len(path) > 4096 || !filepath.IsAbs(path) || filepath.Clean(path) != path ||
			strings.IndexFunc(path, unicode.IsControl) >= 0 {
			return PreparedFiles{}, errors.New("restic: path is not canonical")
		}
	}
	for _, pattern := range excludes {
		if len(pattern) > 4096 || strings.IndexFunc(pattern, unicode.IsControl) >= 0 {
			return PreparedFiles{}, errors.New("restic: exclude pattern is invalid")
		}
	}
	directory, err := os.MkdirTemp("", "restfleet-restic-")
	if err != nil {
		return PreparedFiles{}, err
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		_ = os.RemoveAll(directory)
		return PreparedFiles{}, err
	}
	prepared := PreparedFiles{PathsFile: filepath.Join(directory, "paths"), directory: directory}
	if err := writeLines(prepared.PathsFile, paths); err != nil {
		_ = prepared.Cleanup()
		return PreparedFiles{}, err
	}
	if len(excludes) > 0 {
		prepared.ExcludeFile = filepath.Join(directory, "excludes")
		if err := writeLines(prepared.ExcludeFile, excludes); err != nil {
			_ = prepared.Cleanup()
			return PreparedFiles{}, err
		}
	}
	return prepared, nil
}

func writeLines(path string, values []string) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	writer := bufio.NewWriter(file)
	for _, value := range values {
		if strings.ContainsAny(value, "\x00\r\n") {
			_ = file.Close()
			return errors.New("restic: path contains a line break or NUL")
		}
		if _, err := writer.WriteString(value + "\n"); err != nil {
			_ = file.Close()
			return err
		}
	}
	if err := writer.Flush(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

// Cleanup removes the generated directory and everything in it.
func (p PreparedFiles) Cleanup() error {
	if p.directory == "" {
		return nil
	}
	return os.RemoveAll(p.directory)
}

// BackupArgs returns the exact argv for one backup. Repository and password are
// passed through the environment instead so they never appear in `ps`.
func BackupArgs(request BackupRequest, prepared PreparedFiles) ([]string, error) {
	if request.Repository == "" || request.PasswordFile == "" {
		return nil, errors.New("restic: repository and password file are required")
	}
	if request.HostID == uuid.Nil || request.PlanID == uuid.Nil {
		return nil, errors.New("restic: host and plan ids are required")
	}
	args := []string{
		"backup", "--json",
		"--host", request.HostID.String(),
		"--group-by", "tags",
		"--tag", "restfleet:managed",
		"--tag", "restfleet:host:" + request.HostID.String(),
		"--tag", "restfleet:plan:" + request.PlanID.String(),
		"--files-from-verbatim", prepared.PathsFile,
	}
	if prepared.ExcludeFile != "" {
		args = append(args, "--exclude-file", prepared.ExcludeFile)
	}
	if request.ExcludeCaches {
		args = append(args, "--exclude-caches")
	}
	if request.OneFileSystem {
		args = append(args, "--one-file-system")
	}
	if request.SkipIfUnchanged {
		args = append(args, "--skip-if-unchanged")
	}
	if request.TLSClientCertFile != "" {
		args = append(args, "--tls-client-cert", request.TLSClientCertFile)
	}
	if request.CACertFile != "" {
		args = append(args, "--cacert", request.CACertFile)
	}
	return args, nil
}

// Env is the minimal environment a Restic run needs. Secrets stay in files.
func Env(request BackupRequest) []string {
	return []string{
		"RESTIC_REPOSITORY=" + request.Repository,
		"RESTIC_PASSWORD_FILE=" + request.PasswordFile,
	}
}

// Event is one parsed JSONL message. Unknown message types are ignored.
type Event struct {
	Type        string
	PercentDone float64
	FilesDone   int64
	TotalFiles  int64
	BytesDone   int64
	TotalBytes  int64
	SnapshotID  string
	FilesNew    int64
	DataAdded   int64
	Message     string
}

type wireEvent struct {
	MessageType string   `json:"message_type"`
	PercentDone *float64 `json:"percent_done"`
	FilesDone   *int64   `json:"files_done"`
	TotalFiles  *int64   `json:"total_files"`
	BytesDone   *int64   `json:"bytes_done"`
	TotalBytes  *int64   `json:"total_bytes"`
	SnapshotID  string   `json:"snapshot_id"`
	FilesNew    *int64   `json:"files_new"`
	DataAdded   *int64   `json:"data_added"`
	Message     string   `json:"message"`
}

// ParseEvent decodes one line. The boolean reports whether the line produced a
// usable event; malformed lines are an error so a broken stream is visible.
func ParseEvent(line []byte) (Event, bool, error) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return Event{}, false, nil
	}
	var wire wireEvent
	if err := json.Unmarshal(line, &wire); err != nil {
		return Event{}, false, fmt.Errorf("restic: invalid JSONL message: %w", err)
	}
	switch wire.MessageType {
	case "status":
		event := Event{Type: "status"}
		if wire.PercentDone != nil {
			event.PercentDone = *wire.PercentDone
		}
		if wire.FilesDone != nil {
			event.FilesDone = *wire.FilesDone
		}
		if wire.TotalFiles != nil {
			event.TotalFiles = *wire.TotalFiles
		}
		if wire.BytesDone != nil {
			event.BytesDone = *wire.BytesDone
		}
		if wire.TotalBytes != nil {
			event.TotalBytes = *wire.TotalBytes
		}
		return event, true, nil
	case "summary":
		event := Event{Type: "summary", SnapshotID: wire.SnapshotID}
		if wire.FilesNew != nil {
			event.FilesNew = *wire.FilesNew
		}
		if wire.DataAdded != nil {
			event.DataAdded = *wire.DataAdded
		}
		if wire.FilesDone != nil {
			event.FilesDone = *wire.FilesDone
		}
		return event, true, nil
	case "error":
		return Event{Type: "error", Message: wire.Message}, true, nil
	default:
		return Event{}, false, nil
	}
}

// Run executes Restic in its own process group. On cancellation the whole group
// gets SIGTERM, then SIGKILL after the grace period, so child helpers die too.
type Run struct {
	Binary    string
	Args      []string
	Env       []string
	Stdout    io.Writer
	KillGrace time.Duration
	StderrCap int
}

// Result is the terminal state of one invocation.
type Result struct {
	ExitCode int
	Stderr   string
}

// ErrCanceled marks a run stopped by its context rather than by Restic.
var ErrCanceled = errors.New("restic: run canceled")

func (r Run) Execute(ctx context.Context) (Result, error) {
	binary := r.Binary
	if binary == "" {
		binary = "restic"
	}
	command := exec.Command(binary, r.Args...)
	command.Env = append(os.Environ(), r.Env...)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if r.Stdout != nil {
		command.Stdout = r.Stdout
	}
	limit := r.StderrCap
	if limit <= 0 {
		limit = 8 << 10
	}
	stderr := &limitedBuffer{limit: limit}
	command.Stderr = stderr
	if err := command.Start(); err != nil {
		return Result{}, err
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	select {
	case err := <-done:
		return resultOf(command, stderr.String(), err)
	case <-ctx.Done():
	}
	grace := r.KillGrace
	if grace <= 0 {
		grace = 10 * time.Second
	}
	_ = syscall.Kill(-command.Process.Pid, syscall.SIGTERM)
	select {
	case err := <-done:
		result, _ := resultOf(command, stderr.String(), err)
		return result, ErrCanceled
	case <-time.After(grace):
	}
	_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
	<-done
	return Result{ExitCode: -1, Stderr: stderr.String()}, ErrCanceled
}

func resultOf(command *exec.Cmd, stderr string, err error) (Result, error) {
	if err == nil {
		return Result{ExitCode: 0, Stderr: stderr}, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return Result{ExitCode: exit.ExitCode(), Stderr: stderr}, err
	}
	return Result{ExitCode: -1, Stderr: stderr}, err
}

// limitedBuffer keeps the first limit bytes so error text cannot grow without
// bound. Restic's useful failure line is normally at the start.
type limitedBuffer struct {
	mu        sync.Mutex
	buffer    bytes.Buffer
	limit     int
	truncated bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	remaining := b.limit - b.buffer.Len()
	if remaining <= 0 {
		b.truncated = true
		return len(p), nil
	}
	if len(p) > remaining {
		b.truncated = true
		b.buffer.Write(p[:remaining])
		return len(p), nil
	}
	b.buffer.Write(p)
	return len(p), nil
}

func (b *limitedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.truncated {
		return b.buffer.String() + "…"
	}
	return b.buffer.String()
}
