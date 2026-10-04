package security

import (
	"bytes"
	"encoding/base64"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestProtectedKeyRejectsSymlinksPermissionsLinksFIFOAndContents(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "key")
	key := bytes.Repeat([]byte{8}, 32)
	if os.WriteFile(path, []byte(base64.StdEncoding.EncodeToString(key)+"\n"), 0600) != nil {
		t.Fatal("key fixture")
	}
	got, err := ReadProtectedKey(path, 32)
	if err != nil || !bytes.Equal(got, key) {
		t.Fatal("valid key")
	}
	for _, mode := range []os.FileMode{0644, 0660, 0700, 0600 | os.ModeSetuid, 0600 | os.ModeSetgid, 0600 | os.ModeSticky} {
		if os.Chmod(path, mode) != nil {
			t.Fatal("chmod")
		}
		if _, err = ReadProtectedKey(path, 32); err != ErrGatewayPending {
			t.Fatal("unsafe key mode")
		}
	}
	if os.Chmod(path, 0600) != nil {
		t.Fatal("chmod")
	}
	link := filepath.Join(dir, "link")
	if os.Symlink(path, link) != nil {
		t.Fatal("symlink")
	}
	if _, err = ReadProtectedKey(link, 32); err != ErrGatewayPending {
		t.Fatal("symlink key")
	}
	hard := filepath.Join(dir, "hard")
	if os.Link(path, hard) != nil {
		t.Fatal("hard link")
	}
	if _, err = ReadProtectedKey(path, 32); err != ErrGatewayPending {
		t.Fatal("hard-linked key")
	}
	if os.Remove(hard) != nil {
		t.Fatal("unlink")
	}
	fifo := filepath.Join(dir, "fifo")
	if syscall.Mkfifo(fifo, 0600) != nil {
		t.Fatal("fifo")
	}
	if _, err = ReadProtectedKey(fifo, 32); err != ErrGatewayPending {
		t.Fatal("FIFO key")
	}
	for _, raw := range []string{"secret-canary", base64.StdEncoding.EncodeToString(key[:31]), string(bytes.Repeat([]byte{'a'}, 129))} {
		if os.WriteFile(path, []byte(raw), 0600) != nil {
			t.Fatal("write")
		}
		if _, err = ReadProtectedKey(path, 32); err != ErrGatewayPending {
			t.Fatal("invalid key material")
		}
	}
}
