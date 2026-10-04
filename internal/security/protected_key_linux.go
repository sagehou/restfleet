package security

import (
	"encoding/base64"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// ReadProtectedKey reads a canonical service-owned 0400/0600 regular file.
// Paths, OS errors and input material never enter the returned fixed error.
func ReadProtectedKey(path string, size int) ([]byte, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, ErrGatewayPending
	}
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil || canonical != path {
		return nil, ErrGatewayPending
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, ErrGatewayPending
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || (info.Mode().Perm() != 0600 && info.Mode().Perm() != 0400) || info.Size() > 128 {
		return nil, ErrGatewayPending
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) || stat.Nlink != 1 {
		return nil, ErrGatewayPending
	}
	raw, err := io.ReadAll(io.LimitReader(f, 129))
	defer clear(raw)
	if err != nil || len(raw) > 128 {
		return nil, ErrGatewayPending
	}
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil || len(key) != size {
		clear(key)
		return nil, ErrGatewayPending
	}
	return key, nil
}
