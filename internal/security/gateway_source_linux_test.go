package security

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

func sourceDirectory(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if os.Chmod(dir, 0700) != nil {
		t.Fatal("private source directory")
	}
	return dir
}

func TestGatewaySourceConcurrentCreationNeverReplacesIdentity(t *testing.T) {
	path := filepath.Join(sourceDirectory(t), "source")
	var winners atomic.Int32
	var winner ed25519.PublicKey
	var mu sync.Mutex
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			public, err := CreateGatewaySource(path)
			if err == nil {
				winners.Add(1)
				mu.Lock()
				winner = public
				mu.Unlock()
			} else if err != ErrGatewaySource || public != nil {
				t.Error("unsafe creation error or key output")
			}
		})
	}
	wg.Wait()
	key, err := LoadGatewaySource(path)
	defer clear(key)
	if winners.Load() != 1 || err != nil || !bytes.Equal(key.Public().(ed25519.PublicKey), winner) {
		t.Fatal("concurrent source identity changed")
	}
	before, err := os.ReadFile(path)
	defer clear(before)
	if err != nil {
		t.Fatal("source file")
	}
	if public, err := CreateGatewaySource(path); err != ErrGatewaySource || public != nil {
		t.Fatal("existing identity replaced")
	}
	after, err := os.ReadFile(path)
	defer clear(after)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("rejected creation changed private bytes")
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("source file permissions")
	}
	if _, err = os.Lstat(path + ".pending"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("successful publication retained pending material")
	}
}

func TestGatewaySourceDurabilityFailureRetainsEvidenceWithoutPublishingPublicKey(t *testing.T) {
	for _, failAt := range []int{1, 2, 3} {
		t.Run(string(rune('0'+failAt)), func(t *testing.T) {
			path := filepath.Join(sourceDirectory(t), "source")
			calls := 0
			public, err := createGatewaySource(path, func(f *os.File) error {
				calls++
				if calls == failAt {
					return errors.New("private-fsync-canary")
				}
				return f.Sync()
			})
			if err != ErrGatewaySource || public != nil || calls != failAt {
				t.Fatal("uncertain durable publication exposed a public key or raw error")
			}
			if retry, err := CreateGatewaySource(path); err != ErrGatewaySource || retry != nil {
				t.Fatal("uncertain identity silently replaced")
			}
			if failAt < 3 {
				if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("identity published before required fsync")
				}
				if _, err := os.Stat(path + ".pending"); err != nil {
					t.Fatal("uncertain private evidence discarded")
				}
				if _, err := LoadGatewaySource(path); err != ErrGatewaySource {
					t.Fatal("uncertain pending source automatically recovered")
				}
			} else if _, err := os.Stat(path); err != nil {
				t.Fatal("published but unconfirmed identity discarded")
			}
		})
	}
}

func TestGatewaySourceRejectsUnsafeDirectoryFileAndPendingEvidence(t *testing.T) {
	dir := sourceDirectory(t)
	path := filepath.Join(dir, "source")
	if _, err := CreateGatewaySource(path); err != nil {
		t.Fatal("source fixture")
	}
	for _, mode := range []os.FileMode{0755, 0710, 0770, 0700 | os.ModeSetgid, 0700 | os.ModeSticky} {
		if os.Chmod(dir, mode) != nil {
			t.Fatal("directory fixture")
		}
		if _, err := LoadGatewaySource(path); err != ErrGatewaySource {
			t.Fatal("unsafe source directory loaded")
		}
		if _, err := CreateGatewaySource(filepath.Join(dir, "new")); err != ErrGatewaySource {
			t.Fatal("unsafe source directory initialized")
		}
	}
	if os.Chmod(dir, 0700) != nil {
		t.Fatal("directory reset")
	}
	link := filepath.Join(dir, "link")
	if os.Symlink(path, link) != nil {
		t.Fatal("symlink fixture")
	}
	if _, err := LoadGatewaySource(link); err != ErrGatewaySource {
		t.Fatal("source symlink followed")
	}
	if _, err := CreateGatewaySource(link); err != ErrGatewaySource {
		t.Fatal("source symlink replaced")
	}
	if os.Link(path, path+".pending") != nil {
		t.Fatal("interrupted publication fixture")
	}
	if _, err := LoadGatewaySource(path); err != ErrGatewaySource {
		t.Fatal("incomplete publication loaded")
	}
	if _, err := CreateGatewaySource(path); err != ErrGatewaySource {
		t.Fatal("incomplete publication replaced")
	}
	if _, err := LoadGatewaySource("private-path-canary"); err != ErrGatewaySource {
		t.Fatal("relative source path accepted")
	}
}

func TestGatewayTrustRequiresIndependentProtectedPinAndCopiesKeys(t *testing.T) {
	dir := sourceDirectory(t)
	sourcePath, pinPath := filepath.Join(dir, "source"), filepath.Join(dir, "center.pub")
	public, err := CreateGatewaySource(sourcePath)
	if err != nil {
		t.Fatal("source fixture")
	}
	central, private, err := ed25519.GenerateKey(rand.Reader)
	defer clear(private)
	if err != nil || os.WriteFile(pinPath, []byte(base64.StdEncoding.EncodeToString(central)), 0400) != nil {
		t.Fatal("independently provisioned public pin")
	}
	source, pin, err := LoadGatewayTrust(sourcePath, pinPath)
	defer clear(source)
	if err != nil || !bytes.Equal(source.Public().(ed25519.PublicKey), public) || !bytes.Equal(pin, central) {
		t.Fatal("protected source/pin loading")
	}
	if os.Chmod(pinPath, 0600) != nil || os.WriteFile(pinPath, []byte("private-pin-canary"), 0600) != nil {
		t.Fatal("pin corruption fixture")
	}
	if !bytes.Equal(pin, central) {
		t.Fatal("loaded trust changed with its file")
	}
	if key, pin, err := LoadGatewayTrust(sourcePath, pinPath); err != ErrGatewaySource || key != nil || pin != nil {
		t.Fatal("malformed pin accepted or private copy returned")
	}
	if key, pin, err := LoadGatewayTrust(sourcePath, sourcePath); err != ErrGatewaySource || key != nil || pin != nil {
		t.Fatal("source seed accepted as independent public pin")
	}
}
