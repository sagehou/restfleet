package security

import (
	"bytes"
	"encoding/base64"
	"io"
	"os"
	"path/filepath"
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
	return readProtectedKey(f, size)
}

func readProtectedKey(f *os.File, size int) ([]byte, error) {
	if size < 1 || size > 64 {
		return nil, ErrGatewayPending
	}
	raw, err := readProtectedFile(f, 128)
	defer clear(raw)
	if err != nil {
		return nil, ErrGatewayPending
	}
	encoded := bytes.TrimSpace(raw)
	key := make([]byte, base64.StdEncoding.DecodedLen(len(encoded)))
	n, err := base64.StdEncoding.Decode(key, encoded)
	if err != nil || n != size {
		clear(key)
		return nil, ErrGatewayPending
	}
	return key[:n], nil
}

// ReadProtectedGatewayFile reads bounded local coordinator metadata from a
// private directory under the same ownership/link policy as Gateway identity.
// The caller must validate its versioned content; no path/error is returned.
func ReadProtectedGatewayFile(path string, maxBytes int64) ([]byte, error) {
	if maxBytes < 1 || maxBytes > 64<<10 {
		return nil, ErrGatewayPending
	}
	root, dir, err := gatewayKeyDirectory(path)
	if err != nil {
		return nil, ErrGatewayPending
	}
	defer root.Close()
	defer dir.Close()
	name := filepath.Base(path)
	if _, err := root.Lstat(name + ".pending"); !os.IsNotExist(err) {
		return nil, ErrGatewayPending
	}
	f, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, ErrGatewayPending
	}
	defer f.Close()
	return readProtectedFile(f, maxBytes)
}

func readProtectedFile(f *os.File, maxBytes int64) ([]byte, error) {
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() ||
		(info.Mode().Perm() != 0600 && info.Mode().Perm() != 0400) || info.Size() > maxBytes ||
		info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		return nil, ErrGatewayPending
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) || stat.Nlink != 1 {
		return nil, ErrGatewayPending
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil || int64(len(raw)) > maxBytes {
		clear(raw)
		return nil, ErrGatewayPending
	}
	return raw, nil
}
