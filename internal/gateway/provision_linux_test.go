package gateway

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/sagehou/restfleet/internal/domain"
	"github.com/sagehou/restfleet/internal/security"
)

func provisionConfigFixture() ProvisionConfig {
	uid := uint32(os.Geteuid())
	return ProvisionConfig{Version: 1, Environment: "test", AgentID: uuid.Must(uuid.NewV7()),
		AdmissionLifetimeSeconds: 600, MaterialSocket: "/run/restfleet-gateway/ipc/material.sock",
		AuthoritySocket: "/run/restfleet-gateway/ipc/authority.sock", GatewayUID: uid,
		CentralPinFile: "/var/lib/restfleet-gateway/keys/central.pub",
		AuditSourceFile: "/var/lib/restfleet-gateway/keys/audit.seed",
		RepositorySourceFile: "/var/lib/restfleet-gateway/keys/repo.seed",
		AuditQueueDirectory: "/var/lib/restfleet-gateway/audit-queue",
		QueueDirectory: "/var/lib/restfleet-gateway/repo-queue",
		RuntimeDirectory: "/run/restfleet-gateway/runtime",
		RcloneBinary: "/usr/local/bin/rclone",
		CertificateFile: "/var/lib/restfleet-gateway/tls/server.crt",
		TLSKeyFile: "/var/lib/restfleet-gateway/tls/server.key",
		ClientCAFile: "/var/lib/restfleet-gateway/tls/agent-ca.pem",
		ListenAddress: "127.0.0.1:8443", MaxBytes: security.MaxGatewayPendingSize + (16 << 10),
		MaxRecords: 128, MaxSessions: 4, ServerUID: uid,
		ReplaySocket: "/run/restfleet-ipc/replay.sock", StartupWaitSeconds: 30}
}

func TestProvisionConfigRejectsInvalidStaticPolicy(t *testing.T) {
	c := provisionConfigFixture()
	if !c.validate() {
		t.Fatal("valid provisioning config rejected")
	}
	for _, mutate := range []func(*ProvisionConfig){
		func(c *ProvisionConfig) { c.Version = 2 },
		func(c *ProvisionConfig) { c.Environment = "staging" },
		func(c *ProvisionConfig) { c.AgentID = uuid.Nil },
		func(c *ProvisionConfig) { c.AdmissionLifetimeSeconds = 59 },
		func(c *ProvisionConfig) { c.AdmissionLifetimeSeconds = 2592001 },
		func(c *ProvisionConfig) { c.LifetimeSeconds = security.MaxGatewayAuthorizationSeconds + 1 },
		func(c *ProvisionConfig) { c.AuthoritySocket = c.MaterialSocket },
		func(c *ProvisionConfig) { c.GatewayUID++ },
		func(c *ProvisionConfig) { c.SharedGroup = 123 },
		func(c *ProvisionConfig) { c.MaxBytes = security.MaxGatewayPendingSize + (16 << 10) - 1 },
		func(c *ProvisionConfig) { c.MaxRecords = 0 },
		func(c *ProvisionConfig) { c.MaxRecords = 4097 },
		func(c *ProvisionConfig) { c.MaxSessions = 0 },
		func(c *ProvisionConfig) { c.MaxSessions = 33 },
		func(c *ProvisionConfig) { c.StartupWaitSeconds = 0 },
		func(c *ProvisionConfig) { c.StartupWaitSeconds = 301 },
		func(c *ProvisionConfig) { c.CentralPinFile = c.AuditSourceFile },
		func(c *ProvisionConfig) { c.AuditQueueDirectory = c.QueueDirectory },
		func(c *ProvisionConfig) { c.RuntimeDirectory = "relative-runtime" },
		func(c *ProvisionConfig) { c.ListenAddress = "gateway.example:8443" },
		func(c *ProvisionConfig) { c.ListenAddress = "127.0.0.1:0443" },
	} {
		bad := c
		mutate(&bad)
		if bad.validate() {
			t.Fatal("invalid provisioning policy accepted")
		}
	}
}

func TestProvisionStateProtectedCanonicalMetadata(t *testing.T) {
	config := provisionConfigFixture()
	runtimeID := uuid.Must(uuid.NewV7())
	originID := uuid.Must(uuid.NewV7())
	state := ProvisionState{Version: 1, Config: config, RuntimeID: runtimeID,
		AuditOrigin: domain.GatewayAuditBinding{OriginID: originID, RuntimeID: runtimeID},
		AuditSourcePublic: bytes.Repeat([]byte{1}, 32), RepositorySourcePublic: bytes.Repeat([]byte{2}, 32)}
	dir := t.TempDir()
	if os.Chmod(dir, 0700) != nil {
		t.Fatal("private directory")
	}
	path := filepath.Join(dir, "state.json")
	raw, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if os.WriteFile(path, raw, 0600) != nil {
		t.Fatal("state fixture")
	}
	loaded, err := LoadProvisionState(path)
	if err != nil || loaded.RuntimeID != runtimeID || loaded.AuditOrigin != state.AuditOrigin ||
		!bytes.Equal(loaded.AuditSourcePublic, state.AuditSourcePublic) {
		t.Fatal("protected provisioning state not loaded")
	}
	for _, bad := range [][]byte{
		append(append([]byte(nil), raw...), raw...),
		bytes.Replace(raw, []byte(`"version":1`), []byte(`"version":2`), 1),
		bytes.Replace(raw, []byte(`"version":1`), []byte(`"version":1,"private-secret-canary":true`), 1),
	} {
		if os.WriteFile(path, bad, 0600) != nil {
			t.Fatal("invalid state fixture")
		}
		if _, err := LoadProvisionState(path); err != ErrProvision {
			t.Fatal("ambiguous provisioning state loaded")
		}
	}
	badOrigin := state
	badOrigin.AuditOrigin.RuntimeID = uuid.Must(uuid.NewV7())
	if raw, err = json.Marshal(badOrigin); err != nil {
		t.Fatal(err)
	}
	if os.WriteFile(path, raw, 0600) != nil {
		t.Fatal("origin fixture")
	}
	if _, err := LoadProvisionState(path); err != ErrProvision {
		t.Fatal("mismatched audit origin accepted")
	}
	if os.Chmod(path, 0644) != nil {
		t.Fatal("mode fixture")
	}
	if _, err := LoadProvisionState(path); err != ErrProvision {
		t.Fatal("unsafe state file loaded")
	}
}
