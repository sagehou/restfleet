package gatewaypending

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sagehou/restfleet/internal/security"
	"golang.org/x/crypto/nacl/box"
)

const (
	crossGroup   = 61000
	crossCenter  = 61001
	crossGateway = 61002
	crossOther   = 61003
)

// Fixture provisioning uses inherited anonymous stdin pipes, never argv, env,
// shared secrets files or logs. The Gateway creates its own source key locally.
// This tests Linux isolation and the existing codecs, not central DB admission.
type crossConfig struct {
	Root       string
	Binding    security.GatewayAuthorizationBinding
	CenterPin  ed25519.PublicKey
	SourcePin  ed25519.PublicKey
	CenterKey  ed25519.PrivateKey
	PendingKey []byte
}

func TestCrossUIDMaterialReplayIsolation(t *testing.T) {
	if os.Getenv("RESTFLEET_TEST_CROSS_UID") != "1" {
		t.Skip("Actions explicitly enables privileged test orchestration")
	}
	if os.Geteuid() != 0 {
		t.Fatal("cross-UID orchestration requires root; service children run non-root")
	}
	root, err := os.MkdirTemp("", "rfg-cross-")
	if err != nil {
		t.Fatal("fixture directory")
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	if os.Chmod(root, 0755) != nil {
		t.Fatal("fixture traversal")
	}
	for _, service := range []struct {
		name string
		uid  int
	}{{"center", crossCenter}, {"gateway", crossGateway}} {
		for _, dir := range []struct {
			name string
			mode os.FileMode
		}{{"ipc", 0710}, {"private", 0700}} {
			path := filepath.Join(root, service.name+"-"+dir.name)
			if os.Mkdir(path, dir.mode) != nil || os.Chown(path, service.uid, crossGroup) != nil || os.Chmod(path, dir.mode) != nil {
				t.Fatal("fixture ownership")
			}
		}
	}
	public, central, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal("central fixture key")
	}
	defer clear(central)
	_, pending, err := box.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal("pending fixture key")
	}
	defer clear(pending[:])
	// The trusted fixture coordinator installs ONLY the central public pin in
	// Gateway's private directory; source generation remains inside Gateway.
	pinPath := filepath.Join(root, "gateway-private", "center.pub")
	if os.WriteFile(pinPath, []byte(base64.StdEncoding.EncodeToString(public)), 0600) != nil ||
		os.Chown(pinPath, crossGateway, crossGroup) != nil {
		t.Fatal("independent protected central public pin")
	}
	id := uuid.Must(uuid.NewV7())
	binding := security.GatewayAuthorizationBinding{AdmissionID: id, Owner: id, RuntimeID: id, AgentID: id,
		HostID: id, RepositoryID: id, GatewayID: id, StorageCredentialID: id, DeliveryID: id,
		GatewaySecretRef: id, ResticSecretRef: id, ConfigurationHash: strings.Repeat("a", 64)}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	start := func(role string, uid uint32, config crossConfig) (io.WriteCloser, <-chan error) {
		t.Helper()
		executable, err := os.Executable()
		if err != nil {
			t.Fatal("test executable")
		}
		cmd := exec.CommandContext(ctx, executable, "-test.run=^TestCrossUIDServiceHelper$", "-test.count=1")
		cmd.Env = []string{"RESTFLEET_CROSS_UID_ROLE=" + role}
		cmd.Dir = root
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uid, Gid: crossGroup, Groups: []uint32{crossGroup}}}
		input, err := cmd.StdinPipe()
		if err != nil || cmd.Start() != nil {
			t.Fatal("start isolated service")
		}
		done := make(chan error, 1)
		go func() {
			done <- cmd.Wait()
			close(done)
		}()
		t.Cleanup(func() {
			_ = input.Close()
			_ = cmd.Process.Kill()
			<-done
		})
		if json.NewEncoder(input).Encode(config) != nil {
			t.Fatal("protected fixture delivery")
		}
		return input, done
	}
	waitPath := func(path string) {
		t.Helper()
		for {
			if _, err := os.Lstat(path); err == nil {
				return
			}
			select {
			case <-ctx.Done():
				t.Fatal("isolated service did not become ready")
			case <-time.After(5 * time.Millisecond):
			}
		}
	}
	finish := func(done <-chan error) {
		t.Helper()
		select {
		case err := <-done:
			if err != nil {
				t.Fatal("isolated service failed") // Do not print borrowed fixture contents.
			}
		case <-ctx.Done():
			t.Fatal("isolated service did not join")
		}
	}
	gatewayInput, gatewayDone := start("gateway", crossGateway, crossConfig{Root: root, Binding: binding})
	waitPath(filepath.Join(root, "gateway-ipc", "material.sock"))
	source, err := os.ReadFile(filepath.Join(root, "gateway-ipc", "source.pub"))
	if err != nil || len(source) != ed25519.PublicKeySize {
		t.Fatal("locally generated source proof")
	}
	centerInput, centerDone := start("center", crossCenter, crossConfig{Root: root, Binding: binding, CenterPin: public,
		SourcePin: source, CenterKey: central, PendingKey: pending[:]})
	waitPath(filepath.Join(root, "gateway-ipc", "installed"))
	otherInput, otherDone := start("other", crossOther, crossConfig{Root: root, Binding: binding})
	_ = otherInput.Close()
	finish(otherDone)
	if json.NewEncoder(gatewayInput).Encode(true) != nil {
		t.Fatal("release initialized service")
	}
	finish(gatewayDone)
	_ = gatewayInput.Close()
	_ = centerInput.Close() // Only after Gateway has verified and saved its receipt.
	finish(centerDone)
}

func TestCrossUIDServiceHelper(t *testing.T) {
	role := os.Getenv("RESTFLEET_CROSS_UID_ROLE")
	if role == "" {
		t.Skip("isolated child only")
	}
	if os.Geteuid() == 0 || os.Getegid() != crossGroup {
		t.Fatal("service child is not isolated")
	}
	decoder := json.NewDecoder(os.Stdin)
	var c crossConfig
	if decoder.Decode(&c) != nil {
		t.Fatal("protected fixture input")
	}
	defer clear(c.CenterKey)
	defer clear(c.PendingKey)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	materialPath := filepath.Join(c.Root, "gateway-ipc", "material.sock")
	replayPath := filepath.Join(c.Root, "center-ipc", "replay.sock")
	privatePath := func(service, name string) string { return filepath.Join(c.Root, service+"-private", name) }
	denied := func(context.Context) error { return nil }
	assertPrivate := func(service, name string) {
		t.Helper()
		if _, err := os.ReadFile(privatePath(service, name)); !errors.Is(err, os.ErrPermission) {
			t.Fatal("another service's private material was accessible")
		}
	}
	switch role {
	case "gateway":
		if os.Geteuid() != crossGateway || len(c.CenterKey) != 0 || len(c.PendingKey) != 0 || len(c.CenterPin) != 0 {
			t.Fatal("central secrets reached Gateway")
		}
		public, err := security.CreateGatewaySource(privatePath("gateway", "source"))
		if err != nil {
			t.Fatal("local source key")
		}
		source, centralPin, err := security.LoadGatewayTrust(privatePath("gateway", "source"), privatePath("gateway", "center.pub"))
		if err != nil || !bytes.Equal(source.Public().(ed25519.PublicKey), public) {
			t.Fatal("protected source and independent center pin loading")
		}
		defer clear(source)
		if os.WriteFile(filepath.Join(c.Root, "gateway-ipc", "source.pub"), public, 0644) != nil {
			t.Fatal("local source provisioning")
		}
		receiver, err := NewMaterialReceiverFromFiles(c.Binding, privatePath("gateway", "source"), privatePath("gateway", "center.pub"))
		if err != nil {
			t.Fatal("receiver")
		}
		defer receiver.Close()
		listener, err := ListenReplay(materialPath, crossGroup)
		if err != nil {
			t.Fatal("material listener")
		}
		var queue *Queue
		var borrowed []byte
		if os.Mkdir(privatePath("gateway", "queue"), 0700) != nil {
			t.Fatal("private pending directory")
		}
		err = receiver.Receive(ctx, listener, crossCenter, func(_ context.Context, m security.GatewayMaterial) (func(), error) {
			borrowed = m.Config
			if !bytes.Equal(m.Config, []byte("cross-uid-secret-canary")) {
				return nil, ErrChannel
			}
			queue, err = Create(privatePath("gateway", "queue"), c.Binding, m.PendingRecipient, source, centralPin, Limits{MaxBytes: 2 << 20, MaxRecords: 4})
			if err != nil {
				return nil, ErrQueue
			}
			return func() {
				queue.Freeze()
				_ = queue.Close()
			}, nil
		}, denied, crossGroup)
		if err != nil || queue == nil || len(bytes.Trim(borrowed, "\x00")) != 0 {
			t.Fatal("material initialization and clearing")
		}
		defer queue.Close()
		assertPrivate("center", "signing")
		assertPrivate("center", "pending")
		if err := os.Remove(replayPath); !errors.Is(err, os.ErrPermission) {
			t.Fatal("Gateway could replace center socket")
		}
		if os.WriteFile(filepath.Join(c.Root, "gateway-ipc", "installed"), []byte("ready"), 0644) != nil {
			t.Fatal("installation fixture marker")
		}
		var release bool
		if decoder.Decode(&release) != nil || !release || queue.Append(pendingAudit()) != nil ||
			Drain(ctx, queue, replayPath, crossCenter, c.Binding.RuntimeID, crossGroup) != nil {
			t.Fatal("source-signed cross-UID replay")
		}
	case "center":
		if os.Geteuid() != crossCenter {
			t.Fatal("wrong center UID")
		}
		for name, raw := range map[string][]byte{"signing": c.CenterKey.Seed(), "pending": c.PendingKey} {
			if os.WriteFile(privatePath("center", name), []byte(base64.StdEncoding.EncodeToString(raw)), 0600) != nil {
				t.Fatal("central protected key fixture")
			}
			loaded, err := security.ReadProtectedKey(privatePath("center", name), 32)
			if err != nil || !bytes.Equal(loaded, raw) {
				t.Fatal("protected key loading")
			}
			clear(loaded)
		}
		assertPrivate("gateway", "source")
		assertPrivate("gateway", "center.pub")
		if err := os.Remove(materialPath); !errors.Is(err, os.ErrPermission) {
			t.Fatal("center could replace Gateway socket")
		}
		listener, err := ListenReplay(replayPath, crossGroup)
		if err != nil {
			t.Fatal("replay listener")
		}
		defer listener.Close()
		var effects atomic.Int32
		var rejections atomic.Int32
		done := make(chan error, 1)
		go func() {
			done <- ServeReplay(ctx, listener, crossGateway, func(_ context.Context, runtime uuid.UUID, wire []byte) ([]byte, error) {
				r, err := security.OpenGatewayPending(wire, c.SourcePin, c.PendingKey)
				if err != nil || runtime != c.Binding.RuntimeID || r.Header.Binding != c.Binding || r.Header.Sequence != 1 || effects.Add(1) != 1 {
					return nil, ErrChannel
				}
				h := r.Header
				return security.SignGatewayPendingReceipt(security.GatewayPendingReceipt{AdmissionID: h.Binding.AdmissionID, RuntimeID: runtime,
					Sequence: h.Sequence, RecordID: h.RecordID, WireHash: security.GatewayPendingHash(wire)}, c.CenterKey)
			}, func(context.Context) error { rejections.Add(1); return nil }, crossGroup)
		}()
		err = SendMaterial(ctx, materialPath, crossGateway, c.Binding, c.SourcePin, func(_ context.Context, proof []byte) ([]byte, error) {
			challenge, err := security.VerifyGatewayMaterialChallenge(proof, c.SourcePin, c.Binding)
			if err != nil {
				return nil, err
			}
			now := time.Now().Unix()
			statement, err := security.SignGatewayStatement(security.GatewayStatement{Binding: c.Binding, Revision: 1, IssuedAt: now - 1, ExpiresAt: now + 60}, c.CenterKey)
			if err != nil {
				return nil, err
			}
			recipient, err := security.GatewayPendingPublicKey(c.PendingKey)
			if err != nil {
				return nil, err
			}
			raw := []byte("cross-uid-secret-canary")
			defer clear(raw)
			return security.SealGatewayMaterial(security.GatewayMaterial{Challenge: challenge, Source: c.SourcePin, PendingRecipient: recipient,
				Statement: statement, AdmissionCreatedAt: now - 2, AdmissionExpiresAt: now + 120, SecretRevision: 1,
				Remote: "encrypted", ConfigHash: security.GatewayPendingHash(raw), Config: raw}, c.CenterKey)
		}, denied, crossGroup)
		if err != nil {
			cancel()
			<-done
			t.Fatal("encrypted cross-UID delivery")
		}
		var shutdown bool
		if decoder.Decode(&shutdown) != io.EOF {
			cancel()
			<-done
			t.Fatal("trusted fixture completion")
		}
		cancel()
		if <-done != nil || effects.Load() != 1 || rejections.Load() != 1 {
			t.Fatal("cross-UID replay effect or denial boundary")
		}
	case "other":
		if os.Geteuid() != crossOther {
			t.Fatal("wrong third UID")
		}
		assertPrivate("gateway", "source")
		assertPrivate("gateway", "center.pub")
		assertPrivate("center", "signing")
		assertPrivate("center", "pending")
		if err := os.Remove(replayPath); !errors.Is(err, os.ErrPermission) {
			t.Fatal("group peer could replace listener")
		}
		if _, err := Replay(ctx, replayPath, crossCenter, c.Binding.RuntimeID, []byte("untrusted-record"), crossGroup); err != ErrChannel {
			t.Fatal("wrong UID reached replay handler")
		}
	default:
		t.Fatal("unknown isolated role")
	}
}
