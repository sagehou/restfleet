package main

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
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sagehou/restfleet/internal/domain"
	"github.com/sagehou/restfleet/internal/gatewaypending"
	"github.com/sagehou/restfleet/internal/security"
	"golang.org/x/crypto/nacl/box"
)

func TestReplayCommandDrainsBothDomainsAndPublishesOnlyExactTail(t *testing.T) {
	for _, global := range []bool{false, true} {
		t.Run(map[bool]string{false: "repository", true: "global"}[global], func(t *testing.T) {
			dir, err := os.MkdirTemp("", "rfg-replay-cli-")
			if err != nil {
				t.Fatal("CLI fixture directory")
			}
			t.Cleanup(func() { _ = os.RemoveAll(dir) })
			queuePath := filepath.Join(dir, "queue")
			if os.Mkdir(queuePath, 0700) != nil {
				t.Fatal("CLI private queue")
			}
			public, source, err := ed25519.GenerateKey(rand.Reader)
			if err != nil {
				t.Fatal("source fixture")
			}
			defer clear(source)
			central, signer, err := ed25519.GenerateKey(rand.Reader)
			if err != nil {
				t.Fatal("center fixture")
			}
			defer clear(signer)
			recipient, private, err := box.GenerateKey(rand.Reader)
			if err != nil {
				t.Fatal("recipient fixture")
			}
			defer clear(private[:])
			id := uuid.Must(uuid.NewV7())
			binding := security.GatewayAuthorizationBinding{AdmissionID: id, Owner: id, RuntimeID: id, AgentID: id, HostID: id,
				RepositoryID: id, GatewayID: id, StorageCredentialID: id, DeliveryID: id, GatewaySecretRef: id,
				ResticSecretRef: id, ConfigurationHash: strings.Repeat("a", 64)}
			limits := gatewaypending.Limits{MaxBytes: 2 << 20, MaxRecords: 4}
			config := gatewaypending.ReplayConfig{Version: 1, Binding: binding, SourcePublic: public, RecipientPublic: recipient[:],
				CentralPinFile: filepath.Join(dir, "center.pub"), QueueDirectory: queuePath, MaxBytes: limits.MaxBytes, MaxRecords: limits.MaxRecords,
				SocketPath: filepath.Join(dir, "replay.sock"), ServerUID: uint32(os.Geteuid())}
			record := security.GatewayPendingRecord{Header: security.GatewayPendingHeader{AuthorizationRevision: 1, CreatedAt: time.Now().Unix()},
				Kind: "audit", Event: &domain.GatewayEvent{Action: "denied", Reason: "route_unavailable"}}
			var queue *gatewaypending.Queue
			if global {
				config.Binding = security.GatewayAuthorizationBinding{}
				config.AuditOrigin = security.GatewayAuditBinding{OriginID: id, RuntimeID: id}
				record.Kind, record.Header.AuthorizationRevision = "global_audit", 0
				queue, err = gatewaypending.CreateGlobalAudit(queuePath, config.AuditOrigin, *recipient, source, central, limits)
			} else {
				queue, err = gatewaypending.Create(queuePath, binding, *recipient, source, central, limits)
			}
			if err != nil {
				t.Fatal("CLI offline queue")
			}
			defer queue.Close()
			if queue.Append(record) != nil {
				t.Fatal("CLI offline observation")
			}
			wire, err := queue.Next()
			if err != nil || queue.Close() != nil {
				t.Fatal("CLI recovery fixture")
			}
			path := filepath.Join(dir, "replay.json")
			raw, err := json.Marshal(config)
			if err != nil || os.WriteFile(path, raw, 0400) != nil ||
				os.WriteFile(config.CentralPinFile, []byte(base64.StdEncoding.EncodeToString(central)), 0400) != nil {
				t.Fatal("CLI public metadata")
			}
			listener, err := gatewaypending.ListenReplay(config.SocketPath)
			if err != nil {
				t.Fatal("CLI protected listener")
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			var calls atomic.Int32
			go func() {
				done <- gatewaypending.ServeReplay(ctx, listener, config.ServerUID, func(_ context.Context, runtime uuid.UUID, pending []byte) ([]byte, error) {
					r, err := security.OpenGatewayPending(pending, public, private[:])
					if err != nil || !bytes.Equal(pending, wire) || runtime != id || r.Header.Binding != config.Binding || r.Header.AuditOrigin != config.AuditOrigin {
						return nil, gatewaypending.ErrChannel
					}
					calls.Add(1)
					h := r.Header
					return security.SignGatewayPendingReceipt(security.GatewayPendingReceipt{AdmissionID: config.Binding.AdmissionID, AuditOriginID: config.AuditOrigin.OriginID,
						RuntimeID: runtime, Sequence: h.Sequence, RecordID: h.RecordID, WireHash: security.GatewayPendingHash(pending)}, signer)
				}, func(context.Context) error { return nil })
			}()
			defer func() {
				cancel()
				if <-done != nil {
					t.Error("CLI replay server did not join")
				}
			}()
			var output bytes.Buffer
			if run([]string{"replay", "--config-file", path}, &output) != nil {
				t.Fatal("CLI replay failed")
			}
			var tail gatewaypending.ReplayTail
			if json.Unmarshal(output.Bytes(), &tail) != nil || tail.Version != 1 || tail.Sequence != 1 || tail.WireHash != security.GatewayPendingHash(wire) ||
				tail.Binding != config.Binding || tail.AuditOrigin != config.AuditOrigin {
				t.Fatal("CLI emitted an incorrect tail")
			}
			for _, secret := range [][]byte{source[:32], signer[:32], private[:]} {
				if strings.Contains(output.String(), base64.StdEncoding.EncodeToString(secret)) {
					t.Fatal("CLI leaked a private key")
				}
			}
			if run([]string{"replay", "--config-file", path}, failingOutput{}) != gatewaypending.ErrReplayCommand || calls.Load() != 1 {
				t.Fatal("CLI output failure leaked or repeated effects")
			}
		})
	}
}

func TestReplayCommandRejectsFlagsAndUntrustedFilesWithoutEchoing(t *testing.T) {
	for _, arguments := range [][]string{
		{"replay"},
		{"replay", "--private-canary=secret-canary"},
		{"replay", "--config-file", "private-path-canary"},
		{"replay", "--config-file", "/private-canary", "secret-canary"},
	} {
		var output bytes.Buffer
		err := run(arguments, &output)
		if err != gatewaypending.ErrReplayCommand || strings.Contains(err.Error(), "canary") || output.Len() != 0 {
			t.Fatal("CLI rejection echoed input")
		}
	}
}
