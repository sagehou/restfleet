package restic

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/sagehou/restfleet/internal/rclone"
)

// ErrInvalidListRequest rejects anything outside the fixed `restic ls` scope.
// No caller-supplied flag, repository, or snapshot identifier is interpolated
// into a shell.
var ErrInvalidListRequest = errors.New("invalid snapshot listing request")

const (
	defaultListNodes   = 100_000
	maxListNodes       = 250_000
	defaultListEntries = 2_000
	maxListEntries     = 5_000
	maxListLine        = 1 << 20
)

// ListRequest is borrowed central-only repository material. Passwords and
// rclone config MUST NOT be logged, persisted here, or sent to an Agent.
type ListRequest struct {
	Config       []byte
	Remote       string
	GatewayID    uuid.UUID
	RepositoryID uuid.UUID
	Password     []byte
	SnapshotID   string
	Path         string
	MaxNodes     int
	MaxEntries   int
}

// Node is one direct child of the requested snapshot path.
type Node struct {
	Name    string
	Path    string
	Type    string
	Size    int64
	Mode    string
	UID     int64
	GID     int64
	ModTime time.Time
}

// ListResult is bounded by MaxNodes and MaxEntries. Truncated means the scan
// stopped before Restic finished; it is not an empty or complete directory.
type ListResult struct {
	TreeID    string
	Entries   []Node
	Scanned   int
	Truncated bool
}

func (r *ListRequest) normalize() error {
	if r.GatewayID.Version() != 7 || r.GatewayID.Variant() != uuid.RFC4122 ||
		r.RepositoryID.Version() != 7 || r.RepositoryID.Variant() != uuid.RFC4122 ||
		len(r.Password) < 32 || len(r.Password) > 1024 ||
		bytes.ContainsAny(r.Password, "\x00\r\n") ||
		!repositoryIDPattern.MatchString(r.SnapshotID) {
		return ErrInvalidListRequest
	}
	cleaned, err := normalizeListPath(r.Path)
	if err != nil {
		return err
	}
	r.Path = cleaned
	if r.MaxNodes == 0 {
		r.MaxNodes = defaultListNodes
	}
	if r.MaxNodes < 1 || r.MaxNodes > maxListNodes {
		return ErrInvalidListRequest
	}
	if r.MaxEntries == 0 {
		r.MaxEntries = defaultListEntries
	}
	if r.MaxEntries < 1 || r.MaxEntries > maxListEntries {
		return ErrInvalidListRequest
	}
	return nil
}

func normalizeListPath(raw string) (string, error) {
	if raw == "" {
		return "/", nil
	}
	if len(raw) > 4096 || !strings.HasPrefix(raw, "/") ||
		strings.IndexFunc(raw, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
		return "", ErrInvalidListRequest
	}
	for _, part := range strings.Split(raw, "/") {
		if part == "." || part == ".." {
			return "", ErrInvalidListRequest
		}
	}
	if path.Clean(raw) != raw {
		return "", ErrInvalidListRequest
	}
	return raw, nil
}

// NormalizeListPath is the single canonicalizer shared by the HTTP boundary and
// the Restic adapter. It rejects relative, non-canonical, control-character and
// traversal paths before any job is created.
func NormalizeListPath(raw string) (string, error) {
	return normalizeListPath(raw)
}

// List runs one read-only `restic ls` through the central repository scope and
// returns the direct children of request.Path. Only fixed argv elements are
// passed to Restic; the repository and password arrive through the environment.
func (p *Provisioner) List(ctx context.Context, request ListRequest,
	persist func(context.Context, []byte) error,
) (ListResult, error) {
	if err := request.normalize(); err != nil {
		return ListResult{}, err
	}
	var result ListResult
	var commandErr error
	err := p.runtime.WithConfig(ctx, request.Config, request.Remote, persist,
		func(ctx context.Context, config, binary string) error {
			commandErr = p.withRepository(ctx, config, binary, request.Remote, request.GatewayID, request.RepositoryID, request.Password,
				func(workCtx context.Context, dir, repository, passwordFile string) error {
					var err error
					result, err = p.list(workCtx, dir, repository, passwordFile, request)
					return err
				})
			return commandErr
		})
	if errors.Is(err, rclone.ErrCommandFailed) {
		err = commandErr // Only fixed, sanitized errors cross this boundary.
	}
	if err != nil {
		return ListResult{}, err
	}
	if result.TreeID == "" {
		return ListResult{}, ErrInvalidOutput
	}
	return result, nil
}

func (p *Provisioner) list(ctx context.Context, dir, repository, passwordFile string, request ListRequest) (ListResult, error) {
	workCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := command(workCtx, p.binary, dir, "--json", "--cache-dir", filepath.Join(dir, "cache"),
		"ls", request.SnapshotID, request.Path)
	cmd.Env = append(cmd.Env, "RESTIC_REPOSITORY="+repository, "RESTIC_PASSWORD_FILE="+passwordFile)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return ListResult{}, ErrProcessFailed
	}
	if err := cmd.Start(); err != nil {
		return ListResult{}, ErrProcessFailed
	}
	defer func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }()
	result := ListResult{Entries: make([]Node, 0)}
	if parseErr := parseListJSON(stdout, request.Path, request.MaxNodes, request.MaxEntries, &result); parseErr != nil {
		cancel()
		_ = cmd.Wait()
		if ctx.Err() != nil {
			return ListResult{}, ctx.Err()
		}
		return ListResult{}, parseErr
	}
	if result.Truncated {
		cancel()
		_ = cmd.Wait()
	} else if err := cmd.Wait(); err != nil {
		if ctx.Err() != nil {
			return ListResult{}, ctx.Err()
		}
		return ListResult{}, listExitError(err)
	}
	if ctx.Err() != nil {
		return ListResult{}, ctx.Err()
	}
	return result, nil
}

func listExitError(err error) error {
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		switch exit.ExitCode() {
		case 10:
			return ErrRepositoryAbsent
		case 11:
			return ErrRepositoryLocked
		case 12:
			return ErrWrongPassword
		}
	}
	return ErrProcessFailed // Includes partial exit 3 and unknown codes.
}

// parseListJSON reads Restic's JSONL stream without buffering the whole tree.
// Unknown message types and fields are ignored; malformed node essentials fail.
func parseListJSON(r io.Reader, root string, maxNodes, maxEntries int, result *ListResult) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64<<10), maxListLine)
	entries := make(map[string]Node)
loop:
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var event struct {
			MessageType string `json:"message_type"`
			Tree        string `json:"tree"`
			Name        string `json:"name"`
			Path        string `json:"path"`
			Type        string `json:"type"`
			Size        int64  `json:"size"`
			Mode        uint32 `json:"mode"`
			UID         uint32 `json:"uid"`
			GID         uint32 `json:"gid"`
			MTime       string `json:"mtime"`
		}
		if json.Unmarshal(line, &event) != nil {
			return ErrInvalidOutput
		}
		switch event.MessageType {
		case "snapshot":
			if event.Tree == "" || result.TreeID != "" || !repositoryIDPattern.MatchString(event.Tree) {
				return ErrInvalidOutput
			}
			result.TreeID = event.Tree
		case "node":
			result.Scanned++
			if result.Scanned > maxNodes {
				result.Truncated = true
				break loop
			}
			if event.Name == "" || len(event.Name) > 255 || event.Type == "" || event.MTime == "" {
				return ErrInvalidOutput
			}
			nodePath := strings.TrimSuffix(event.Path, "/")
			if nodePath == "" {
				nodePath = "/"
			}
			if !strings.HasPrefix(nodePath, "/") || len(nodePath) > 4096 ||
				strings.IndexFunc(nodePath, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
				return ErrInvalidOutput
			}
			mtime, err := time.Parse(time.RFC3339Nano, event.MTime)
			if err != nil {
				return ErrInvalidOutput
			}
			name, ok := directChild(root, nodePath)
			if !ok {
				continue
			}
			if name != event.Name {
				return ErrInvalidOutput
			}
			if _, exists := entries[nodePath]; exists {
				continue
			}
			if len(entries) >= maxEntries {
				result.Truncated = true
				break loop
			}
			entries[nodePath] = Node{
				Name: name, Path: nodePath, Type: event.Type, Size: event.Size,
				Mode: fmt.Sprintf("%04o", event.Mode&0o7777), UID: int64(event.UID), GID: int64(event.GID),
				ModTime: mtime.UTC(),
			}
		default:
			// Forward-compatible: Restic may add message types.
		}
	}
	if err := scanner.Err(); err != nil {
		return ErrInvalidOutput
	}
	result.Entries = result.Entries[:0]
	for _, node := range entries {
		result.Entries = append(result.Entries, node)
	}
	sort.Slice(result.Entries, func(i, j int) bool { return result.Entries[i].Name < result.Entries[j].Name })
	return nil
}

func directChild(parent, nodePath string) (string, bool) {
	if parent == "/" {
		name := strings.TrimPrefix(nodePath, "/")
		if name == "" || strings.Contains(name, "/") {
			return "", false
		}
		return name, true
	}
	name, ok := strings.CutPrefix(nodePath, parent+"/")
	if !ok || name == "" || strings.Contains(name, "/") {
		return "", false
	}
	return name, true
}
