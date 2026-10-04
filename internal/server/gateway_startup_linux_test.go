package server

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/sagehou/restfleet/internal/security"
)

func startupConfigFixture() GatewayStartupConfig {
	id := uuid.Must(uuid.NewV7())
	return GatewayStartupConfig{Version: 1, Binding: security.GatewayAuthorizationBinding{AdmissionID: id, Owner: id, RuntimeID: id,
		AgentID: id, HostID: id, RepositoryID: id, GatewayID: id, StorageCredentialID: id, DeliveryID: id,
		GatewaySecretRef: id, ResticSecretRef: id, ConfigurationHash: strings.Repeat("a", 64)},
		SourcePublic: bytes.Repeat([]byte{3}, 32), DecisionID: id, LifetimeSeconds: 600,
		SocketPath: "/run/restfleet-gateway/ipc/material.sock", GatewayUID: uint32(os.Geteuid())}
}

func TestGatewayStartupProtectedCanonicalMetadata(t *testing.T) {
	s := startupConfigFixture()
	dir := t.TempDir()
	if os.Chmod(dir, 0700) != nil {
		t.Fatal("private directory")
	}
	path := filepath.Join(dir, "startup.json")
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var pretty bytes.Buffer
	if json.Indent(&pretty, raw, "", "  ") != nil || os.WriteFile(path, pretty.Bytes(), 0400) != nil {
		t.Fatal("metadata fixture")
	}
	loaded, err := LoadGatewayStartupConfig(path)
	if err != nil || loaded.Binding != s.Binding || !bytes.Equal(loaded.SourcePublic, s.SourcePublic) || loaded.DecisionID != s.DecisionID {
		t.Fatal("protected metadata not loaded")
	}
	if os.Chmod(path, 0600) != nil {
		t.Fatal("writable fixture")
	}
	for _, bad := range [][]byte{
		append(append([]byte(nil), raw...), raw...),
		bytes.Replace(raw, []byte(`"version":1`), []byte(`"version":1,"version":1`), 1),
		bytes.Replace(raw, []byte(`"version":1`), []byte(`"version":2`), 1),
		bytes.Replace(raw, []byte(`"version":1`), []byte(`"Version":1`), 1),
		bytes.Replace(raw, []byte(`"version":1`), []byte(`"version":1,"private-secret-canary":true`), 1),
		bytes.Replace(raw, []byte(`,"shared_group":0`), nil, 1),
		bytes.Replace(raw, []byte(`"shared_group":0`), []byte(`"shared_group":null`), 1),
		bytes.Replace(raw, []byte(`"lifetime_seconds":600`), []byte(`"lifetime_seconds":43201`), 1),
		bytes.Replace(raw, []byte(`"lifetime_seconds":600`), []byte(`"lifetime_seconds":0`), 1),
		[]byte("private-config-canary"), bytes.Repeat([]byte{' '}, 4097),
	} {
		if os.WriteFile(path, bad, 0600) != nil {
			t.Fatal("invalid metadata fixture")
		}
		if got, err := LoadGatewayStartupConfig(path); err != ErrGatewayStartup || got.SourcePublic != nil || got.Binding.AdmissionID != uuid.Nil {
			t.Fatal("ambiguous metadata loaded or unsafe error/output")
		}
	}
	if os.WriteFile(path, raw, 0600) != nil {
		t.Fatal("fixture reset")
	}
	for _, mode := range []os.FileMode{0644, 0660, 0600 | os.ModeSetgid, 0600 | os.ModeSticky} {
		if os.Chmod(path, mode) != nil {
			t.Fatal("mode fixture")
		}
		if _, err := LoadGatewayStartupConfig(path); err != ErrGatewayStartup {
			t.Fatal("unsafe file loaded")
		}
	}
	if os.Chmod(path, 0600) != nil {
		t.Fatal("file mode reset")
	}
	for _, mode := range []os.FileMode{0755, 0710, 0700 | os.ModeSetgid} {
		if os.Chmod(dir, mode) != nil {
			t.Fatal("directory mode fixture")
		}
		if _, err := LoadGatewayStartupConfig(path); err != ErrGatewayStartup {
			t.Fatal("unsafe directory loaded")
		}
	}
	if os.Chmod(dir, 0700) != nil {
		t.Fatal("directory reset")
	}
	link := filepath.Join(dir, "linked.json")
	if os.Symlink(path, link) != nil {
		t.Fatal("symlink fixture")
	}
	if _, err := LoadGatewayStartupConfig(link); err != ErrGatewayStartup {
		t.Fatal("symlink followed")
	}
	hardlink := filepath.Join(dir, "hardlink.json")
	if os.Link(path, hardlink) != nil {
		t.Fatal("hardlink fixture")
	}
	if _, err := LoadGatewayStartupConfig(path); err != ErrGatewayStartup {
		t.Fatal("hardlinked metadata loaded")
	}
	if os.Remove(hardlink) != nil {
		t.Fatal("hardlink reset")
	}
	if os.Link(path, path+".pending") != nil {
		t.Fatal("pending fixture")
	}
	if _, err := LoadGatewayStartupConfig(path); err != ErrGatewayStartup {
		t.Fatal("pending/hardlinked metadata loaded")
	}
	if _, err := LoadGatewayStartupConfig("private-path-canary"); err != ErrGatewayStartup {
		t.Fatal("relative metadata loaded")
	}
}

func TestGatewayStartupRejectsInvalidBindingSourceAndChannelPolicy(t *testing.T) {
	s := startupConfigFixture()
	for _, mutate := range []func(*GatewayStartupConfig){
		func(s *GatewayStartupConfig) { s.Binding.RuntimeID = uuid.Nil },
		func(s *GatewayStartupConfig) { s.SourcePublic = make([]byte, 64) },
		func(s *GatewayStartupConfig) { s.DecisionID = uuid.Nil },
		func(s *GatewayStartupConfig) { s.SocketPath = "/run/../private-path-canary" },
		func(s *GatewayStartupConfig) { s.SocketPath = "relative-path" },
		func(s *GatewayStartupConfig) { s.SocketPath = "/run/private\x00path" },
		func(s *GatewayStartupConfig) { s.SocketPath = "/" + strings.Repeat("a", 107) },
		func(s *GatewayStartupConfig) { s.GatewayUID = ^uint32(0) },
		func(s *GatewayStartupConfig) { s.GatewayUID++; s.SharedGroup = 0 },
		func(s *GatewayStartupConfig) { s.SharedGroup = ^uint32(0) },
		func(s *GatewayStartupConfig) { s.SharedGroup = 123 },
	} {
		bad := s
		mutate(&bad)
		if bad.Validate() == nil {
			t.Fatal("invalid startup policy accepted")
		}
	}
}
