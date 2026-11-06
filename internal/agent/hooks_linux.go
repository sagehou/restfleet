package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/sagehou/restfleet/internal/agent/restic"
	"github.com/sagehou/restfleet/internal/domain"
	"github.com/sagehou/restfleet/internal/security"
)

type LocalHook struct {
	ID             uuid.UUID `json:"id"`
	Executable     string    `json:"executable"`
	Args           []string  `json:"args"`
	TimeoutSeconds int       `json:"timeout_seconds"`
}

var errHookConfig = errors.New("local Hook allowlist is invalid or unavailable")

func LoadHooks(path string) (map[uuid.UUID]LocalHook, error) {
	hooks := make(map[uuid.UUID]LocalHook)
	if path == "" {
		return hooks, nil
	}
	raw, err := security.ReadProtectedGatewayFile(path, 64<<10)
	if err != nil {
		return nil, errHookConfig
	}
	defer clear(raw)
	var config struct {
		Version int         `json:"version"`
		Hooks   []LocalHook `json:"hooks"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&config) != nil || config.Version != 1 || len(config.Hooks) > 64 {
		return nil, errHookConfig
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return nil, errHookConfig
	}
	for _, hook := range config.Hooks {
		if hook.ID.Version() != 7 || hook.ID.Variant() != uuid.RFC4122 || hook.TimeoutSeconds < 1 || hook.TimeoutSeconds > 3600 ||
			len(hook.Args) > 64 || !safeHookExecutable(hook.Executable) {
			return nil, errHookConfig
		}
		if _, exists := hooks[hook.ID]; exists {
			return nil, errHookConfig
		}
		for _, arg := range hook.Args {
			if len(arg) > 4096 || strings.IndexFunc(arg, unicode.IsControl) >= 0 {
				return nil, errHookConfig
			}
		}
		hooks[hook.ID] = hook
	}
	return hooks, nil
}

func safeHookExecutable(path string) bool {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return false
	}
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil || canonical != path {
		return false
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 || info.Mode().Perm()&0022 != 0 || info.Mode()&(os.ModeSetuid|os.ModeSetgid) != 0 {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || (stat.Uid != 0 && stat.Uid != uint32(os.Geteuid())) {return false}
	for directory:=filepath.Dir(path);;directory=filepath.Dir(directory){
		parent,err:=os.Lstat(directory)
		if err!=nil || !parent.IsDir(){return false}
		owner,ok:=parent.Sys().(*syscall.Stat_t)
		if !ok || (owner.Uid!=0 && owner.Uid!=uint32(os.Geteuid())){return false}
		// A root-owned sticky temporary directory protects its trusted child's
		// name; every other writable ancestor could replace the executable.
		if parent.Mode().Perm()&0022!=0 && !(owner.Uid==0 && parent.Mode()&os.ModeSticky!=0){return false}
		if directory==filepath.Dir(directory){break}
	}
	return true
}

func (s *State) validateHooks(execution *domain.BackupExecution) bool {
	if execution == nil {
		return true
	}
	for _, ids := range [][]uuid.UUID{execution.PreHookIDs, execution.PostHookIDs} {
		for _, id := range ids {
			if _, ok := s.hooks[id]; !ok {
				return false
			}
		}
	}
	return true
}

func (s *State) runHooks(ctx context.Context, ids []uuid.UUID) error {
	for _, id := range ids {
		hook, ok := s.hooks[id]
		if !ok || !safeHookExecutable(hook.Executable) {
			return errHookConfig
		}
		hookCtx, cancel := context.WithTimeout(ctx, time.Duration(hook.TimeoutSeconds)*time.Second)
		result, err := (restic.Run{Binary: hook.Executable, Args: hook.Args, KillGrace: time.Second}).Execute(hookCtx)
		cancel()
		// Raw Hook output is never logged or uploaded. Only the section's fixed
		// outcome is recorded by the executor.
		if err != nil || result.ExitCode != 0 {
			return errors.New("local Hook execution failed")
		}
	}
	return nil
}
