package restic

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/sagehou/restfleet/internal/rclone"
)

var (
	ErrInvalidProvision = errors.New("invalid repository provisioning request")
	ErrProcessFailed    = errors.New("repository command failed")
	ErrBackendFailed    = errors.New("repository backend failed")
	ErrRepositoryAbsent = errors.New("repository does not exist")
	ErrRepositoryLocked = errors.New("repository is locked")
	ErrWrongPassword    = errors.New("repository password rejected")
	ErrInvalidOutput    = errors.New("invalid repository command output")
	ErrRepositoryMatch  = errors.New("repository identity or format mismatch")
	ErrRepositoryInUse  = errors.New("provisioning target already contains snapshots")
)

var repositoryIDPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

const maxCommandOutput = 64 << 10

// ProvisionRequest is borrowed central-only secret material: never log, persist,
// or send this struct to an Agent. IDs and password MUST come from the durable
// provisioning record, not be regenerated on retry.
type ProvisionRequest struct {
	Config       []byte
	Remote       string
	GatewayID    uuid.UUID
	RepositoryID uuid.UUID
	Password     []byte
	ExpectedID   string // Native Restic ID, if already recorded by a previous attempt.
}

// RepositoryInfo is engine metadata, not a READY state or Agent acknowledgement.
type RepositoryInfo struct {
	ID            string
	FormatVersion int
}

// Provisioner only implements init/config/empty-snapshot verification. The
// central job worker MUST fence it with a per-repository lease, exclude backups,
// and audit secret access before invoking it. No generic command API is exposed.
type Provisioner struct {
	binary  string
	runtime *rclone.Runtime
}

func NewProvisioner(binary string, runtime *rclone.Runtime) (*Provisioner, error) {
	if runtime == nil || !filepath.IsAbs(binary) || filepath.Clean(binary) != binary {
		return nil, ErrInvalidProvision
	}
	info, err := os.Stat(binary)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 ||
		info.Mode().Perm()&0022 != 0 {
		return nil, ErrInvalidProvision
	}
	return &Provisioner{binary: binary, runtime: runtime}, nil
}

func (p *Provisioner) Provision(ctx context.Context, request ProvisionRequest,
	persist func(context.Context, []byte) error,
) (RepositoryInfo, error) {
	if request.GatewayID.Version() != 7 || request.GatewayID.Variant() != uuid.RFC4122 ||
		request.RepositoryID.Version() != 7 || request.RepositoryID.Variant() != uuid.RFC4122 ||
		len(request.Password) < 32 || len(request.Password) > 1024 ||
		bytes.IndexAny(request.Password, "\x00\r\n") >= 0 ||
		(request.ExpectedID != "" && !repositoryIDPattern.MatchString(request.ExpectedID)) {
		return RepositoryInfo{}, ErrInvalidProvision
	}
	var result RepositoryInfo
	var commandErr error
	err := p.runtime.WithConfig(ctx, request.Config, request.Remote, persist,
		func(ctx context.Context, filename, binary string) error {
			result, commandErr = p.provision(ctx, request, filename, binary)
			return commandErr
		})
	if errors.Is(err, rclone.ErrCommandFailed) {
		err = commandErr // Only our fixed, sanitized errors can cross this boundary.
	}
	if err != nil {
		return RepositoryInfo{}, err
	}
	return result, nil
}

func (p *Provisioner) provision(ctx context.Context, request ProvisionRequest, config, rcloneBinary string) (RepositoryInfo, error) {
	dir := filepath.Dir(config)
	socket := filepath.Join(dir, "rest.sock")
	// Linux sockaddr_un is bounded; ':' separates the socket from the REST path.
	if len(socket) >= 108 || bytes.ContainsAny([]byte(socket), ":\x00") {
		return RepositoryInfo{}, ErrInvalidProvision
	}
	passwordFile := filepath.Join(dir, "password")
	file, err := os.OpenFile(passwordFile, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return RepositoryInfo{}, rclone.ErrUnsafeRuntime
	}
	_, writeErr := file.Write(request.Password)
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil || os.Mkdir(filepath.Join(dir, "cache"), 0700) != nil {
		return RepositoryInfo{}, rclone.ErrUnsafeRuntime
	}
	// This full-access backend has no TCP listener or public route. Both children
	// are owned directly, so cancellation cannot orphan Restic's rclone backend.
	workCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	backendPath := request.Remote + ":restfleet/agents/" + request.GatewayID.String() + "/" + request.RepositoryID.String()
	backend := command(workCtx, rcloneBinary, dir, "serve", "restic", backendPath,
		"--config", config, "--addr", socket, "--cache-objects=false",
		"--retries", "1", "--low-level-retries", "1", "--contimeout", "10s", "--timeout", "30s")
	backend.Stdout = io.Discard
	if backend.Start() != nil {
		return RepositoryInfo{}, ErrBackendFailed
	}
	stopped := make(chan struct{})
	go func() {
		_ = backend.Wait()
		cancel()
		close(stopped)
	}()
	defer func() {
		cancel()
		<-stopped
		_ = syscall.Kill(-backend.Process.Pid, syscall.SIGKILL)
	}()
	if err := waitSocket(workCtx, socket); err != nil {
		if ctx.Err() != nil {
			return RepositoryInfo{}, ctx.Err()
		}
		return RepositoryInfo{}, ErrBackendFailed
	}

	endpoint := &url.URL{Scheme: "http+unix", Path: socket + ":/"}
	repository := "rest:" + endpoint.String()
	info, err := p.initialize(workCtx, dir, repository, passwordFile, request.ExpectedID)
	if ctx.Err() != nil {
		return RepositoryInfo{}, ctx.Err()
	}
	if workCtx.Err() != nil {
		return RepositoryInfo{}, ErrBackendFailed
	}
	return info, err
}

func waitSocket(ctx context.Context, socket string) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		info, err := os.Lstat(socket)
		if err == nil {
			if info.Mode()&os.ModeSocket == 0 || os.Chmod(socket, 0600) != nil {
				return ErrBackendFailed
			}
			dialer := net.Dialer{Timeout: time.Second}
			conn, err := dialer.DialContext(ctx, "unix", socket)
			if err == nil {
				_ = conn.Close()
				return nil
			}
		} else if !os.IsNotExist(err) {
			return ErrBackendFailed
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// initialize never retries a write after an ambiguous error. A later durable
// attempt probes using the SAME password and scope, then resumes verification.
func (p *Provisioner) initialize(ctx context.Context, dir, repository, passwordFile, expected string) (RepositoryInfo, error) {
	readConfig := func() (RepositoryInfo, error) {
		raw, err := p.run(ctx, dir, repository, passwordFile, "cat", "config")
		defer clear(raw)
		if err != nil {
			return RepositoryInfo{}, err
		}
		var config struct {
			ID      string `json:"id"`
			Version *int   `json:"version"`
		}
		if json.Unmarshal(raw, &config) != nil || !repositoryIDPattern.MatchString(config.ID) || config.Version == nil {
			return RepositoryInfo{}, ErrInvalidOutput
		}
		if *config.Version != 2 || (expected != "" && config.ID != expected) {
			return RepositoryInfo{}, ErrRepositoryMatch
		}
		return RepositoryInfo{ID: config.ID, FormatVersion: *config.Version}, nil
	}
	info, err := readConfig()
	if errors.Is(err, ErrRepositoryAbsent) && expected == "" {
		raw, initErr := p.run(ctx, dir, repository, passwordFile, "init", "--repository-version", "2")
		defer clear(raw)
		if initErr != nil {
			return RepositoryInfo{}, initErr
		}
		id, err := initializedID(raw)
		if err != nil {
			return RepositoryInfo{}, err
		}
		expected = id
		info, err = readConfig()
		if err != nil {
			return RepositoryInfo{}, err
		}
	} else if err != nil {
		return RepositoryInfo{}, err
	}
	raw, err := p.run(ctx, dir, repository, passwordFile, "snapshots")
	defer clear(raw)
	if err != nil {
		return RepositoryInfo{}, err
	}
	var snapshots []json.RawMessage
	if json.Unmarshal(raw, &snapshots) != nil || snapshots == nil {
		return RepositoryInfo{}, ErrInvalidOutput
	}
	if len(snapshots) != 0 {
		return RepositoryInfo{}, ErrRepositoryInUse
	}
	return info, nil
}

func initializedID(raw []byte) (string, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	id := ""
	for {
		var event struct {
			Type string `json:"message_type"`
			ID   string `json:"id"`
		}
		err := decoder.Decode(&event)
		if err == io.EOF {
			break
		}
		if err != nil || event.Type == "" {
			return "", ErrInvalidOutput
		}
		if event.Type == "initialized" {
			if id != "" || !repositoryIDPattern.MatchString(event.ID) {
				return "", ErrInvalidOutput
			}
			id = event.ID
		}
	}
	if id == "" {
		return "", ErrInvalidOutput
	}
	return id, nil
}

func command(ctx context.Context, binary, dir string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Dir = dir
	cmd.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin", "LANG=C", "TMPDIR=" + dir}
	cmd.Stderr = io.Discard
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = 2 * time.Second
	return cmd
}

func (p *Provisioner) run(ctx context.Context, dir, repository, passwordFile string, args ...string) ([]byte, error) {
	argv := append([]string{"--json", "--cache-dir", filepath.Join(dir, "cache")}, args...)
	cmd := command(ctx, p.binary, dir, argv...)
	cmd.Env = append(cmd.Env, "RESTIC_REPOSITORY="+repository, "RESTIC_PASSWORD_FILE="+passwordFile)
	output := &commandOutput{}
	cmd.Stdout = output
	if cmd.Start() != nil {
		return nil, ErrProcessFailed
	}
	defer func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }()
	err := cmd.Wait()
	if ctx.Err() != nil {
		clear(output.data)
		return nil, ctx.Err()
	}
	if err != nil {
		clear(output.data)
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			switch exit.ExitCode() {
			case 10:
				return nil, ErrRepositoryAbsent
			case 11:
				return nil, ErrRepositoryLocked
			case 12:
				return nil, ErrWrongPassword
			}
		}
		return nil, ErrProcessFailed // Includes partial exit 3 and unknown codes.
	}
	if output.overflow {
		clear(output.data)
		return nil, ErrInvalidOutput
	}
	return output.data, nil
}

type commandOutput struct {
	data     []byte
	overflow bool
}

func (b *commandOutput) Write(p []byte) (int, error) {
	n := len(p)
	if remaining := maxCommandOutput - len(b.data); len(p) > remaining {
		b.overflow = true
		p = p[:remaining]
	}
	b.data = append(b.data, p...)
	return n, nil
}
