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

func rotationConfigFixture() GatewayRotationConfig {
	id := uuid.Must(uuid.NewV7())
	next := uuid.Must(uuid.NewV7())
	binding := security.GatewayAuthorizationBinding{AdmissionID: id, Owner: id, RuntimeID: id,
		AgentID: id, HostID: id, RepositoryID: id, GatewayID: id, StorageCredentialID: id, DeliveryID: id,
		GatewaySecretRef: id, ResticSecretRef: id, ConfigurationHash: strings.Repeat("a", 64)}
	uid := uint32(os.Geteuid())
	return GatewayRotationConfig{Version: 1,
		Current: GatewayAuthorityTarget{Binding: binding, SourcePublic: bytes.Repeat([]byte{2}, 32),
			SocketPath: "/run/restfleet-gateway/ipc/authority.sock", GatewayUID: uid},
		Next: GatewayPreparation{Version: 1, AgentID: id, RuntimeID: next, SourcePublic: bytes.Repeat([]byte{3}, 32),
			AdmissionLifetimeSeconds: 600, MaterialSocket: "/run/restfleet-gateway/ipc/material.sock",
			AuthoritySocket: "/run/restfleet-gateway/ipc/next-authority.sock", GatewayUID: uid},
		PlanFile: "/var/lib/restfleet/gateway-rotation/plan.json",
		AuditConfigFile: "/var/lib/restfleet/gateway-rotation/audit.json", WaitSeconds: 60}
}

func TestGatewayRotationProtectedCanonicalMetadata(t *testing.T) {
	r := rotationConfigFixture()
	dir := t.TempDir()
	if os.Chmod(dir, 0700) != nil {
		t.Fatal("private directory")
	}
	path := filepath.Join(dir, "rotation.json")
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var pretty bytes.Buffer
	if json.Indent(&pretty, raw, "", "  ") != nil || os.WriteFile(path, pretty.Bytes(), 0400) != nil {
		t.Fatal("metadata fixture")
	}
	loaded, err := LoadGatewayRotationConfig(path)
	if err != nil || loaded.Current.Binding != r.Current.Binding || loaded.PlanFile != r.PlanFile ||
		!bytes.Equal(loaded.Next.SourcePublic, r.Next.SourcePublic) || loaded.Next.RuntimeID != r.Next.RuntimeID {
		t.Fatal("protected rotation metadata not loaded")
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
		bytes.Replace(raw, []byte(`"wait_seconds":60`), []byte(`"wait_seconds":0`), 1),
		bytes.Replace(raw, []byte(`"wait_seconds":60`), []byte(`"wait_seconds":301`), 1),
		bytes.Replace(raw, []byte(`"plan_file":"/var/lib/restfleet/gateway-rotation/plan.json"`),
			[]byte(`"plan_file":"relative-plan.json"`), 1),
		bytes.Replace(raw, []byte(`"plan_file":"/var/lib/restfleet/gateway-rotation/plan.json"`),
			[]byte(`"plan_file":"/var/lib/restfleet/gateway-rotation/audit.json"`), 1),
		[]byte("private-config-canary"), bytes.Repeat([]byte{' '}, 16385),
	} {
		if os.WriteFile(path, bad, 0600) != nil {
			t.Fatal("invalid metadata fixture")
		}
		if got, err := LoadGatewayRotationConfig(path); err != ErrGatewayRotation || got.Current.Binding.AdmissionID != uuid.Nil {
			t.Fatal("ambiguous rotation metadata loaded or unsafe error/output")
		}
	}
	if os.WriteFile(path, raw, 0600) != nil {
		t.Fatal("fixture reset")
	}
	for _, mode := range []os.FileMode{0644, 0660, 0600 | os.ModeSetgid, 0600 | os.ModeSticky} {
		if os.Chmod(path, mode) != nil {
			t.Fatal("mode fixture")
		}
		if _, err := LoadGatewayRotationConfig(path); err != ErrGatewayRotation {
			t.Fatal("unsafe rotation file loaded")
		}
	}
}

func TestGatewayRotationRejectsInvalidBindingAndNextRuntime(t *testing.T) {
	r := rotationConfigFixture()
	for _, mutate := range []func(*GatewayRotationConfig){
		func(r *GatewayRotationConfig) { r.Current.Binding.RuntimeID = uuid.Nil },
		func(r *GatewayRotationConfig) { r.Current.SourcePublic = make([]byte, 31) },
		func(r *GatewayRotationConfig) { r.Next.RuntimeID = r.Current.Binding.RuntimeID },
		func(r *GatewayRotationConfig) { r.Next.AgentID = uuid.Must(uuid.NewV7()) },
		func(r *GatewayRotationConfig) { r.Next.MaterialSocket = r.Next.AuthoritySocket },
		func(r *GatewayRotationConfig) { r.Next.AdmissionLifetimeSeconds = 59 },
		func(r *GatewayRotationConfig) { r.Next.LifetimeSeconds = security.MaxGatewayAuthorizationSeconds + 1 },
		func(r *GatewayRotationConfig) { r.PlanFile = r.AuditConfigFile },
		func(r *GatewayRotationConfig) { r.AuditConfigFile = "relative-audit.json" },
	} {
		bad := r
		mutate(&bad)
		if bad.valid("/var/lib/restfleet/gateway-rotation/rotation.json") {
			t.Fatal("invalid rotation policy accepted")
		}
	}
}

func recoveryConfigFixture() GatewayRecoveryConfig {
	id := uuid.Must(uuid.NewV7())
	return GatewayRecoveryConfig{Version: 1,
		Binding: security.GatewayAuthorizationBinding{AdmissionID: id, Owner: id, RuntimeID: id,
			AgentID: id, HostID: id, RepositoryID: id, GatewayID: id, StorageCredentialID: id, DeliveryID: id,
			GatewaySecretRef: id, ResticSecretRef: id, ConfigurationHash: strings.Repeat("b", 64)},
		SourcePublic: bytes.Repeat([]byte{4}, 32)}
}

func TestGatewayRecoveryProtectedCanonicalMetadata(t *testing.T) {
	r := recoveryConfigFixture()
	dir := t.TempDir()
	if os.Chmod(dir, 0700) != nil {
		t.Fatal("private directory")
	}
	path := filepath.Join(dir, "recovery.json")
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if os.WriteFile(path, raw, 0600) != nil {
		t.Fatal("metadata fixture")
	}
	loaded, err := LoadGatewayRecoveryConfig(path)
	if err != nil || loaded.Binding != r.Binding || !bytes.Equal(loaded.SourcePublic, r.SourcePublic) {
		t.Fatal("protected recovery metadata not loaded")
	}
	for _, bad := range [][]byte{
		append(append([]byte(nil), raw...), raw...),
		bytes.Replace(raw, []byte(`"version":1`), []byte(`"version":2`), 1),
		bytes.Replace(raw, []byte(`"version":1`), []byte(`"version":1,"private-secret-canary":true`), 1),
		bytes.Replace(raw, []byte(`"source_public":"`) , []byte(`"source_public":"AAAA`), 1),
		[]byte("private-config-canary"),
	} {
		if os.WriteFile(path, bad, 0600) != nil {
			t.Fatal("invalid metadata fixture")
		}
		if _, err := LoadGatewayRecoveryConfig(path); err != ErrGatewayRecovery {
			t.Fatal("ambiguous recovery metadata loaded")
		}
	}
	if os.WriteFile(path, raw, 0600) != nil {
		t.Fatal("fixture reset")
	}
	for _, mode := range []os.FileMode{0644, 0660, 0600 | os.ModeSetgid} {
		if os.Chmod(path, mode) != nil {
			t.Fatal("mode fixture")
		}
		if _, err := LoadGatewayRecoveryConfig(path); err != ErrGatewayRecovery {
			t.Fatal("unsafe recovery file loaded")
		}
	}
}

func TestGatewayRecoveryAllowsOmittedSourceOnly(t *testing.T) {
	r := recoveryConfigFixture()
	r.SourcePublic = nil
	if !r.valid() {
		t.Fatal("omitted source rejected")
	}
	r.SourcePublic = make([]byte, 32)
	if r.valid() {
		t.Fatal("all-zero source accepted")
	}
	r.SourcePublic = bytes.Repeat([]byte{1}, 31)
	if r.valid() {
		t.Fatal("short source accepted")
	}
	r.Version = 2
	if r.valid() {
		t.Fatal("invalid version accepted")
	}
}
