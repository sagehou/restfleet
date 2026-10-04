package security

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

var ErrGatewaySource = errors.New("gateway source identity unavailable or inconsistent")

// CreateGatewaySource creates a NEW local source seed and returns ONLY its
// public key after file and directory fsync. It never replaces an old identity.
// Failed or interrupted publication retains private evidence for trusted review.
func CreateGatewaySource(path string) (ed25519.PublicKey, error) {
	return createGatewaySource(path, func(f *os.File) error { return f.Sync() })
}

func createGatewaySource(path string, syncFile func(*os.File) error) (ed25519.PublicKey, error) {
	root, dir, err := gatewayKeyDirectory(path)
	if err != nil {
		return nil, ErrGatewaySource
	}
	defer root.Close()
	defer dir.Close()
	name := filepath.Base(path)
	if _, err := root.Lstat(name); !errors.Is(err, os.ErrNotExist) {
		return nil, ErrGatewaySource
	}
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, ErrGatewaySource
	}
	defer clear(private)
	raw := make([]byte, base64.StdEncoding.EncodedLen(ed25519.SeedSize)+1)
	defer clear(raw)
	base64.StdEncoding.Encode(raw[:len(raw)-1], private[:ed25519.SeedSize])
	raw[len(raw)-1] = '\n'
	pending := name + ".pending"
	f, err := root.OpenFile(pending, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, ErrGatewaySource
	}
	defer f.Close()
	// Another creator may have completed between the first check and O_EXCL.
	// This file is still our empty, unwritten reservation, not recovery evidence.
	if _, err := root.Lstat(name); !errors.Is(err, os.ErrNotExist) {
		if f.Close() == nil && root.Remove(pending) == nil {
			_ = syncFile(dir)
		}
		return nil, ErrGatewaySource
	}
	if f.Chmod(0600) != nil {
		return nil, ErrGatewaySource
	}
	if n, err := f.Write(raw); err != nil || n != len(raw) || syncFile(f) != nil || f.Close() != nil || syncFile(dir) != nil {
		return nil, ErrGatewaySource
	}
	// Native no-replace publication: Link fails if any destination appeared.
	// Until the private pending link is removed, protected readers reject Nlink=2.
	if root.Link(pending, name) != nil || root.Remove(pending) != nil || syncFile(dir) != nil {
		return nil, ErrGatewaySource
	}
	return public, nil
}

// LoadGatewaySource never generates a replacement, repairs a pending file or
// recovers authority. The caller MUST clear the returned private key after use.
func LoadGatewaySource(path string) (ed25519.PrivateKey, error) {
	seed, err := readGatewayKey(path)
	if err != nil {
		return nil, ErrGatewaySource
	}
	defer clear(seed)
	return ed25519.NewKeyFromSeed(seed), nil
}

// LoadGatewayTrust loads the local source seed and independently provisioned
// central PUBLIC pin. Neither is taken from a peer, material, argv or env value.
// Both files require a service-owned private directory. Clear source after use.
func LoadGatewayTrust(sourceFile, centralPinFile string) (ed25519.PrivateKey, ed25519.PublicKey, error) {
	if sourceFile == centralPinFile {
		return nil, nil, ErrGatewaySource
	}
	source, err := LoadGatewaySource(sourceFile)
	if err != nil {
		return nil, nil, ErrGatewaySource
	}
	pin, err := readGatewayKey(centralPinFile)
	if err != nil {
		clear(source)
		return nil, nil, ErrGatewaySource
	}
	return source, ed25519.PublicKey(pin), nil
}

func readGatewayKey(path string) ([]byte, error) {
	root, dir, err := gatewayKeyDirectory(path)
	if err != nil {
		return nil, ErrGatewaySource
	}
	defer root.Close()
	defer dir.Close()
	name := filepath.Base(path)
	if _, err := root.Lstat(name + ".pending"); !errors.Is(err, os.ErrNotExist) {
		return nil, ErrGatewaySource
	}
	f, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, ErrGatewaySource
	}
	defer f.Close()
	key, err := readProtectedKey(f, ed25519.SeedSize)
	if err != nil {
		return nil, ErrGatewaySource
	}
	return key, nil
}

func gatewayKeyDirectory(path string) (*os.Root, *os.File, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || filepath.Base(path) == "." || filepath.Base(path) == "/" {
		return nil, nil, ErrGatewaySource
	}
	directory := filepath.Dir(path)
	canonical, err := filepath.EvalSymlinks(directory)
	if err != nil || canonical != directory {
		return nil, nil, ErrGatewaySource
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, nil, ErrGatewaySource
	}
	dir, err := root.Open(".")
	if err != nil {
		_ = root.Close()
		return nil, nil, ErrGatewaySource
	}
	info, err := dir.Stat()
	if err == nil && info.IsDir() && info.Mode().Perm() == 0700 && info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) == 0 {
		if stat, ok := info.Sys().(*syscall.Stat_t); ok && stat.Uid == uint32(os.Geteuid()) {
			return root, dir, nil
		}
	}
	_ = dir.Close()
	_ = root.Close()
	return nil, nil, ErrGatewaySource
}
