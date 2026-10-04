package gatewaypending

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sagehou/restfleet/internal/security"
	"golang.org/x/crypto/nacl/box"
)

func replayMetadata(t *testing.T, f queueFixture) (ReplayConfig, string) {
	t.Helper()
	dir := t.TempDir()
	if os.Chmod(dir, 0700) != nil {
		t.Fatal("private replay metadata directory")
	}
	pin := filepath.Join(dir, "center.pub")
	if os.WriteFile(pin, []byte(base64.StdEncoding.EncodeToString(f.confirmation.Public().(ed25519.PublicKey))), 0400) != nil {
		t.Fatal("independent central public pin")
	}
	return ReplayConfig{Version: 1, Binding: f.binding, SourcePublic: f.public, RecipientPublic: bytes.Clone(f.recipient[:]),
		CentralPinFile: pin, QueueDirectory: f.path, MaxBytes: f.limits.MaxBytes, MaxRecords: f.limits.MaxRecords,
		SocketPath: replaySocket(t), ServerUID: uint32(os.Geteuid())}, filepath.Join(dir, "replay.json")
}

func TestReplayMetadataCanonicalContentAndProtectedFiles(t *testing.T) {
	f := newQueueFixture(t)
	s, path := replayMetadata(t, f)
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal("metadata encoding")
	}
	var pretty bytes.Buffer
	if json.Indent(&pretty, raw, "", "  ") != nil || os.WriteFile(path, pretty.Bytes(), 0400) != nil {
		t.Fatal("metadata fixture")
	}
	got, err := LoadReplayConfig(path)
	if err != nil || got.Binding != s.Binding || got.SocketPath != s.SocketPath {
		t.Fatal("protected pretty metadata rejected")
	}
	if os.Chmod(path, 0600) != nil {
		t.Fatal("metadata writable fixture")
	}
	for _, content := range []string{
		strings.Replace(string(raw), `"version":1`, `"version":1,"version":1`, 1),
		strings.Replace(string(raw), `"version":1`, `"Version":1`, 1),
		strings.Replace(string(raw), `"version":1`, `"version":1e0`, 1),
		strings.Replace(string(raw), `"version":1`, `"version":null`, 1),
		strings.Replace(string(raw), `"version":1,`, "", 1),
		strings.Replace(string(raw), `"version":1`, `"version":1,"private_canary":"secret-canary"`, 1),
		strings.Replace(string(raw), `"shared_group":0`, `"shared_group":null`, 1),
		string(raw) + string(raw),
	} {
		if os.WriteFile(path, []byte(content), 0600) != nil {
			t.Fatal("malformed metadata fixture")
		}
		if got, err := LoadReplayConfig(path); err != ErrReplayCommand || got.Version != 0 {
			t.Fatal("malformed metadata accepted or echoed")
		}
	}
	if os.WriteFile(path, raw, 0600) != nil || os.Chmod(path, 0644) != nil {
		t.Fatal("unsafe metadata mode")
	}
	if _, err := LoadReplayConfig(path); err != ErrReplayCommand {
		t.Fatal("public metadata file accepted")
	}
	if os.Chmod(path, 0600) != nil || os.Symlink(path, path+".link") != nil || os.Link(path, path+".hard") != nil {
		t.Fatal("unsafe metadata links")
	}
	for _, bad := range []string{path, path + ".link", path + ".hard", "relative-canary"} {
		if _, err := LoadReplayConfig(bad); err != ErrReplayCommand {
			t.Fatal("unsafe metadata path accepted")
		}
	}
}

func TestReplayConfigRejectsMixedDomainsPeerPolicyAndUnsafeLimits(t *testing.T) {
	f := newQueueFixture(t)
	s, _ := replayMetadata(t, f)
	for _, mutate := range []func(*ReplayConfig){
		func(s *ReplayConfig) { s.Version = 2 },
		func(s *ReplayConfig) { s.Binding.RuntimeID = uuid.Nil },
		func(s *ReplayConfig) {
			s.AuditOrigin = security.GatewayAuditBinding{OriginID: f.binding.AdmissionID, RuntimeID: f.binding.RuntimeID}
		},
		func(s *ReplayConfig) { s.SourcePublic = nil },
		func(s *ReplayConfig) { s.RecipientPublic = make([]byte, 32) },
		func(s *ReplayConfig) { s.MaxBytes = security.MaxGatewayPendingSize },
		func(s *ReplayConfig) { s.MaxBytes = 64<<20 + 1 },
		func(s *ReplayConfig) { s.MaxRecords = 0 },
		func(s *ReplayConfig) { s.MaxRecords = 4097 },
		func(s *ReplayConfig) { s.QueueDirectory = "/private-canary/../other" },
		func(s *ReplayConfig) { s.CentralPinFile = "relative-canary" },
		func(s *ReplayConfig) { s.SocketPath = "/private-canary\x00" },
		func(s *ReplayConfig) { s.SocketPath = "/" + strings.Repeat("a", 107) },
		func(s *ReplayConfig) { s.ServerUID = ^uint32(0) },
		func(s *ReplayConfig) { s.ServerUID++ },
		func(s *ReplayConfig) { s.SharedGroup = ^uint32(0) },
		func(s *ReplayConfig) { s.SharedGroup = 61000 },
	} {
		bad := s
		mutate(&bad)
		if bad.Validate() != ErrReplayCommand {
			t.Fatal("unsafe replay metadata accepted")
		}
	}
	s.Binding = security.GatewayAuthorizationBinding{}
	s.AuditOrigin = security.GatewayAuditBinding{OriginID: f.binding.AdmissionID, RuntimeID: f.binding.RuntimeID}
	if s.Validate() != nil {
		t.Fatal("independent global metadata rejected")
	}
	s.AuditOrigin.RuntimeID = uuid.Nil
	if s.Validate() != ErrReplayCommand {
		t.Fatal("partial global identity accepted")
	}
}

func TestRecoveryReplayPreservesLostReceiptAndUsesPublicKeysOnly(t *testing.T) {
	for _, global := range []bool{false, true} {
		t.Run(map[bool]string{false: "repository", true: "global"}[global], func(t *testing.T) {
			f := newQueueFixture(t)
			recipient, private, err := box.GenerateKey(rand.Reader)
			if err != nil {
				t.Fatal("recipient fixture")
			}
			defer clear(private[:])
			f.recipient = *recipient
			s, path := replayMetadata(t, f)
			var q *Queue
			record := pendingAudit()
			central := f.confirmation.Public().(ed25519.PublicKey)
			if global {
				s.Binding = security.GatewayAuthorizationBinding{}
				s.AuditOrigin = security.GatewayAuditBinding{OriginID: f.binding.AdmissionID, RuntimeID: f.binding.RuntimeID}
				q, err = CreateGlobalAudit(f.path, s.AuditOrigin, f.recipient, f.source, central, f.limits)
				record.Kind, record.Header.AuthorizationRevision = "global_audit", 0
			} else {
				q, err = Create(f.path, f.binding, f.recipient, f.source, central, f.limits)
			}
			if err != nil || q.Append(record) != nil {
				t.Fatal("offline record fixture")
			}
			original, err := q.Next()
			if err != nil || q.Close() != nil {
				t.Fatal("crash close fixture")
			}
			// No source seed or recipient private file exists in Gateway metadata.
			clear(f.source)
			raw, err := json.Marshal(s)
			if err != nil || os.WriteFile(path, raw, 0400) != nil {
				t.Fatal("public replay configuration")
			}
			loaded, err := LoadReplayConfig(path)
			if err != nil {
				t.Fatal("load public replay configuration")
			}
			var mu sync.Mutex
			calls, effects := 0, 0
			_, _ = serveReplay(t, s.SocketPath, s.ServerUID, func(_ context.Context, runtime uuid.UUID, wire []byte) ([]byte, error) {
				mu.Lock()
				defer mu.Unlock()
				r, err := security.OpenGatewayPending(wire, f.public, private[:])
				if err != nil || !bytes.Equal(wire, original) || runtime != f.binding.RuntimeID || r.Header.Binding != s.Binding || r.Header.AuditOrigin != s.AuditOrigin {
					return nil, ErrChannel
				}
				calls++
				if effects == 0 {
					effects++
				}
				if calls == 1 {
					return nil, ErrChannel
				} // Commit happened; the exact ACK was lost.
				h := r.Header
				return security.SignGatewayPendingReceipt(security.GatewayPendingReceipt{AdmissionID: s.Binding.AdmissionID, AuditOriginID: s.AuditOrigin.OriginID,
					RuntimeID: runtime, Sequence: h.Sequence, RecordID: h.RecordID, WireHash: security.GatewayPendingHash(wire)}, f.confirmation)
			})
			if tail, err := ReplayFromConfig(context.Background(), loaded); err != ErrReplayCommand || tail.Version != 0 {
				t.Fatal("lost receipt reported a drained tail")
			}
			onDisk, err := os.ReadFile(filepath.Join(f.path, recordName(1)))
			if err != nil || !bytes.Equal(onDisk, original) {
				t.Fatal("uncertain committed record discarded or re-encrypted")
			}
			tail, err := ReplayFromConfig(context.Background(), loaded)
			if err != nil || tail.Version != 1 || tail.Sequence != 1 || tail.WireHash != security.GatewayPendingHash(original) || tail.Binding != s.Binding || tail.AuditOrigin != s.AuditOrigin {
				t.Fatal("exact acknowledged recovery tail")
			}
			again, err := ReplayFromConfig(context.Background(), loaded)
			if err != nil || again != tail {
				t.Fatal("empty recovery did not report the same tail")
			}
			mu.Lock()
			defer mu.Unlock()
			if calls != 2 || effects != 1 {
				t.Fatal("replay duplicated the committed effect")
			}
		})
	}
}

func TestRecoveryReplayRejectsLiveQueueChangedPinsCorruptionAndCancellation(t *testing.T) {
	f := newQueueFixture(t)
	s, _ := replayMetadata(t, f)
	q := f.create(t)
	if q.Append(pendingAudit()) != nil {
		t.Fatal("offline record")
	}
	wire, err := q.Next()
	if err != nil {
		t.Fatal("offline wire")
	}
	var calls atomic.Int32
	_, _ = serveReplay(t, s.SocketPath, s.ServerUID, func(context.Context, uuid.UUID, []byte) ([]byte, error) { calls.Add(1); return nil, ErrChannel })
	if _, err := ReplayFromConfig(context.Background(), s); err != ErrReplayCommand {
		t.Fatal("live producer taken over")
	}
	if q.Close() != nil {
		t.Fatal("queue close")
	}
	for _, mutate := range []func(*ReplayConfig){
		func(s *ReplayConfig) { s.Binding.RuntimeID = uuid.Must(uuid.NewV7()) },
		func(s *ReplayConfig) { s.Binding.Owner = uuid.Must(uuid.NewV7()) },
		func(s *ReplayConfig) { s.SourcePublic = f.confirmation.Public().(ed25519.PublicKey) },
		func(s *ReplayConfig) { s.RecipientPublic = bytes.Repeat([]byte{1}, 32) },
		func(s *ReplayConfig) { s.MaxRecords++ },
	} {
		bad := s
		mutate(&bad)
		if _, err := ReplayFromConfig(context.Background(), bad); err != ErrReplayCommand {
			t.Fatal("changed stored identity recovered")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ReplayFromConfig(ctx, s); err != ErrReplayCommand {
		t.Fatal("canceled recovery succeeded")
	}
	if os.Chmod(s.CentralPinFile, 0600) != nil || os.WriteFile(s.CentralPinFile, []byte(base64.StdEncoding.EncodeToString(f.public)), 0600) != nil {
		t.Fatal("changed center pin fixture")
	}
	if _, err := ReplayFromConfig(context.Background(), s); err != ErrReplayCommand {
		t.Fatal("changed center pin recovered")
	}
	if os.WriteFile(s.CentralPinFile, []byte(base64.StdEncoding.EncodeToString(f.confirmation.Public().(ed25519.PublicKey))), 0600) != nil {
		t.Fatal("restore pin fixture")
	}
	if os.WriteFile(filepath.Join(f.path, "pending.tmp"), []byte("uncertain-secret-canary"), 0600) != nil {
		t.Fatal("uncertain write fixture")
	}
	if _, err := ReplayFromConfig(context.Background(), s); err != ErrReplayCommand {
		t.Fatal("uncertain temporary evidence repaired")
	}
	retained, err := os.ReadFile(filepath.Join(f.path, recordName(1)))
	if err != nil || !bytes.Equal(retained, wire) || calls.Load() != 0 {
		t.Fatal("rejected recovery altered or dispatched evidence")
	}
	if _, err := os.Lstat(filepath.Join(f.path, "pending.tmp")); err != nil {
		t.Fatal("uncertain evidence removed")
	}
	if os.Remove(filepath.Join(f.path, "pending.tmp")) != nil {
		t.Fatal("reset uncertain fixture")
	}
	tampered := bytes.Clone(wire)
	tampered[len(tampered)-1] ^= 1
	if os.WriteFile(filepath.Join(f.path, recordName(1)), tampered, 0600) != nil {
		t.Fatal("corrupt record fixture")
	}
	if _, err := ReplayFromConfig(context.Background(), s); err != ErrReplayCommand {
		t.Fatal("corrupt source wire recovered")
	}
	retained, err = os.ReadFile(filepath.Join(f.path, recordName(1)))
	if err != nil || !bytes.Equal(retained, tampered) || calls.Load() != 0 {
		t.Fatal("corrupt evidence dispatched or discarded")
	}
}

func TestRecoveryReplayCancellationClosesExchangeAndRetainsExactWire(t *testing.T) {
	f := newQueueFixture(t)
	s, _ := replayMetadata(t, f)
	q := f.create(t)
	if q.Append(pendingAudit()) != nil {
		t.Fatal("offline record")
	}
	wire, err := q.Next()
	if err != nil || q.Close() != nil {
		t.Fatal("recovery fixture")
	}
	entered, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	_, _ = serveReplay(t, s.SocketPath, s.ServerUID, func(ctx context.Context, _ uuid.UUID, _ []byte) ([]byte, error) {
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
		}
		return nil, ErrChannel
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := ReplayFromConfig(ctx, s); done <- err }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("recovery exchange did not start")
	}
	cancel()
	select {
	case err := <-done:
		if err != ErrReplayCommand {
			t.Fatal("canceled recovery succeeded")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("canceled exchange did not join")
	}
	retained, err := os.ReadFile(filepath.Join(f.path, recordName(1)))
	if err != nil || !bytes.Equal(retained, wire) {
		t.Fatal("cancellation discarded or changed wire")
	}
	recovered := f.recover(t)
	if recovered.CheckProducer(f.binding, f.public, f.confirmation.Public().(ed25519.PublicKey)) != ErrQueue {
		t.Fatal("canceled recovery revived producer")
	}
}
