package gatewaypending

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sagehou/restfleet/internal/domain"
	"github.com/sagehou/restfleet/internal/security"
	"golang.org/x/crypto/nacl/box"
)

func TestGlobalQueueSeparationExactReceiptAndReplayOnlyRecovery(t *testing.T) {
	f := newQueueFixture(t)
	b := security.GatewayAuditBinding{OriginID: f.binding.AdmissionID, RuntimeID: f.binding.RuntimeID}
	central := f.confirmation.Public().(ed25519.PublicKey)
	q, err := CreateGlobalAudit(f.path, b, f.recipient, f.source, central, f.limits)
	if err != nil { t.Fatal(err) }
	defer q.Close()
	if q.ClaimGlobalAuditProducer(b, f.public, central) != nil || q.ClaimGlobalAuditProducer(b, f.public, central) != ErrQueue ||
		q.CheckProducer(f.binding, f.public, central) != ErrQueue || q.ClaimProducer(f.binding, f.public, central) != ErrQueue {
		t.Fatal("global queue created backup authority or multiple producers")
	}
	if q.Append(pendingAudit()) != ErrQueue { t.Fatal("global source accepted repository audit") }
	r := security.GatewayPendingRecord{Header: security.GatewayPendingHeader{CreatedAt: time.Now().Unix()}, Kind: "global_audit",
		Event: &domain.GatewayEvent{Action: "denied", Reason: "route_unavailable"}}
	if q.Append(r) != nil { t.Fatal("global append") }
	wire, err := q.Next()
	h, verifyErr := security.InspectGatewayPending(wire, f.public)
	if err != nil || verifyErr != nil || h.AuditOrigin != b || h.Binding != (security.GatewayAuthorizationBinding{}) || h.Sequence != 1 {
		t.Fatal("global queue assigned repository identity")
	}
	ack := security.GatewayPendingReceipt{AuditOriginID: b.OriginID, RuntimeID: b.RuntimeID, Sequence: 1, RecordID: h.RecordID, WireHash: security.GatewayPendingHash(wire)}
	for _, bad := range []security.GatewayPendingReceipt{
		{AdmissionID: f.binding.AdmissionID, RuntimeID: b.RuntimeID, Sequence: 1, RecordID: h.RecordID, WireHash: ack.WireHash},
		{AuditOriginID: uuid.Must(uuid.NewV7()), RuntimeID: b.RuntimeID, Sequence: 1, RecordID: h.RecordID, WireHash: ack.WireHash},
		{AuditOriginID: b.OriginID, RuntimeID: b.RuntimeID, Sequence: 1, RecordID: h.RecordID, WireHash: security.GatewayPendingHash([]byte("wrong"))},
	} {
		signed, err := security.SignGatewayPendingReceipt(bad, f.confirmation)
		if err != nil || q.Acknowledge(signed) != ErrQueue { t.Fatal("wrong mode/origin/hash receipt reclaimed record") }
		if next, err := q.Next(); err != nil || !bytes.Equal(next, wire) { t.Fatal("rejected receipt changed pending evidence") }
	}
	signed, err := security.SignGatewayPendingReceipt(ack, f.confirmation)
	if err != nil || q.Acknowledge(signed) != nil { t.Fatal("exact receipt") }
	q.Freeze()
	if seq, hash, err := q.Tail(); err != nil || seq != 1 || hash != ack.WireHash { t.Fatal("frozen tail") }
	if q.Close() != nil { t.Fatal("close") }
	recovered, err := RecoverGlobalAudit(f.path, b, f.recipient, f.public, central, f.limits)
	if err != nil { t.Fatal(err) }
	defer recovered.Close()
	if recovered.Append(r) != ErrQueue || recovered.ClaimGlobalAuditProducer(b, f.public, central) != ErrQueue { t.Fatal("recovery revived audit producer") }
	if recovered.Close() != nil { t.Fatal("recovery close") }
	if _, err := Recover(f.path, f.binding, f.recipient, f.public, central, f.limits); err != ErrQueue { t.Fatal("audit identity reopened as repository") }
}

func TestGlobalQueueEncryptedCapacityAndUncertainWriteRetainEvidence(t *testing.T) {
	for _, failure := range []string{"capacity", "write"} {
		t.Run(failure, func(t *testing.T) {
			f := newQueueFixture(t)
			b := security.GatewayAuditBinding{OriginID: uuid.Must(uuid.NewV7()), RuntimeID: f.binding.RuntimeID}
			f.limits.MaxRecords = 1
			q, err := CreateGlobalAudit(f.path, b, f.recipient, f.source, f.confirmation.Public().(ed25519.PublicKey), f.limits)
			if err != nil { t.Fatal(err) }
			defer q.Close()
			if failure == "write" { q.syncFile = func(*os.File) error { return ErrQueue } }
			r := security.GatewayPendingRecord{Header: security.GatewayPendingHeader{CreatedAt: time.Now().Unix()}, Kind: "global_audit",
				Event: &domain.GatewayEvent{Action: "channel_denied", Reason: "authority_rejected"}}
			err = q.Append(r)
			if failure == "capacity" {
				if err != nil || q.Append(r) != ErrQueue { t.Fatal("capacity bound") }
				wire, err := q.Next()
				if err != nil || bytes.Contains(wire, []byte("authority_rejected")) { t.Fatal("plaintext event on disk") }
			} else {
				if err != ErrQueue { t.Fatal("uncertain fsync succeeded") }
				if _, err := os.Lstat(filepath.Join(f.path, "pending.tmp")); err != nil { t.Fatal("uncertain evidence removed") }
				q.Close()
				if _, err := RecoverGlobalAudit(f.path, b, f.recipient, f.public, f.confirmation.Public().(ed25519.PublicKey), f.limits); err != ErrQueue { t.Fatal("uncertain source recovered") }
			}
		})
	}
}

func TestGlobalQueueUsesEncryptedWireOverExistingReplayChannel(t *testing.T) {
	f := newQueueFixture(t)
	b := security.GatewayAuditBinding{OriginID: uuid.Must(uuid.NewV7()), RuntimeID: f.binding.RuntimeID}
	recipient, private, err := box.GenerateKey(rand.Reader)
	if err != nil { t.Fatal(err) }
	defer clear(private[:])
	q, err := CreateGlobalAudit(f.path, b, *recipient, f.source, f.confirmation.Public().(ed25519.PublicKey), f.limits)
	if err != nil { t.Fatal(err) }
	defer q.Close()
	if q.Append(security.GatewayPendingRecord{Header: security.GatewayPendingHeader{CreatedAt: time.Now().Unix()}, Kind: "global_audit",
		Event: &domain.GatewayEvent{Action: "denied", Reason: "rate_limited"}}) != nil { t.Fatal("append") }
	path := replaySocket(t)
	_, _ = serveReplay(t, path, uint32(os.Geteuid()), func(_ context.Context, runtime uuid.UUID, wire []byte) ([]byte, error) {
		r, err := security.OpenGatewayPending(wire, f.public, private[:])
		if err != nil || runtime != b.RuntimeID || r.Header.AuditOrigin != b || r.Kind != "global_audit" { return nil, ErrChannel }
		h := r.Header
		return security.SignGatewayPendingReceipt(security.GatewayPendingReceipt{AuditOriginID: b.OriginID, RuntimeID: runtime,
			Sequence: h.Sequence, RecordID: h.RecordID, WireHash: security.GatewayPendingHash(wire)}, f.confirmation)
	})
	q.Freeze()
	if Drain(context.Background(), q, path, uint32(os.Geteuid()), b.RuntimeID) != nil { t.Fatal("global replay") }
	if seq, _, err := q.Tail(); err != nil || seq != 1 { t.Fatal("global replay acknowledgement") }
}
