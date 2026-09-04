package agent

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestStateVolumePreservesInstallIdentityAndPrivateKey(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "agent-state")
	first, err := OpenState(directory)
	if err != nil {
		t.Fatal(err)
	}
	installID, err := first.InstallID()
	if err != nil {
		t.Fatal(err)
	}
	privateKey, err := LoadOrCreatePrivateKey(first)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second, err := OpenState(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	reopenedID, err := second.InstallID()
	if err != nil {
		t.Fatal(err)
	}
	reopenedKey, err := LoadOrCreatePrivateKey(second)
	if err != nil {
		t.Fatal(err)
	}
	if installID != reopenedID || !bytes.Equal(privateKey, reopenedKey) {
		t.Fatal("retained state volume changed the Agent install identity")
	}
	for _, check := range []struct {
		path string
		mode os.FileMode
	}{
		{directory, 0o700},
		{filepath.Join(directory, "state.db"), 0o600},
		{filepath.Join(directory, privateKeyFile), 0o600},
	} {
		info, err := os.Stat(check.path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != check.mode {
			t.Fatalf("%s mode = %o, want %o", check.path, info.Mode().Perm(), check.mode)
		}
	}
}

func TestCreateCSRNeverContainsPrivateKey(t *testing.T) {
	state, err := OpenState(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	privateKey, err := LoadOrCreatePrivateKey(state)
	if err != nil {
		t.Fatal(err)
	}
	csr, err := CreateCSR(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(csr, []byte("PRIVATE KEY")) {
		t.Fatal("CSR contains private key material")
	}
}
