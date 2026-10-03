package gatewaypending

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sagehou/restfleet/internal/domain"
	"github.com/sagehou/restfleet/internal/security"
	"golang.org/x/crypto/nacl/box"
)

type queueFixture struct {
	path                 string
	binding              security.GatewayAuthorizationBinding
	public               ed25519.PublicKey
	source, confirmation ed25519.PrivateKey
	recipient            [32]byte
	limits               Limits
}

func newQueueFixture(t *testing.T) queueFixture {
	t.Helper()
	public, source, _ := ed25519.GenerateKey(rand.Reader)
	_, confirmation, _ := ed25519.GenerateKey(rand.Reader)
	recipient, _, _ := box.GenerateKey(rand.Reader)
	id := uuid.Must(uuid.NewV7())
	b := security.GatewayAuthorizationBinding{AdmissionID: id, Owner: id, RuntimeID: id, AgentID: id, HostID: id, RepositoryID: id, GatewayID: id, StorageCredentialID: id, DeliveryID: id, GatewaySecretRef: id, ResticSecretRef: id, ConfigurationHash: strings.Repeat("a", 64)}
	path := t.TempDir()
	if os.Chmod(path, 0700) != nil {
		t.Fatal("private test directory")
	}
	return queueFixture{path, b, public, source, confirmation, *recipient, Limits{MaxBytes: 2 << 20, MaxRecords: 4}}
}

func (f queueFixture) create(t *testing.T) *Queue {
	t.Helper()
	q, err := Create(f.path, f.binding, f.recipient, f.source, f.confirmation.Public().(ed25519.PublicKey), f.limits)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = q.Close() })
	return q
}

func (f queueFixture) recover(t *testing.T) *Queue {
	t.Helper()
	q, err := Recover(f.path, f.binding, f.recipient, f.public, f.confirmation.Public().(ed25519.PublicKey), f.limits)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = q.Close() })
	return q
}

func pendingAudit() security.GatewayPendingRecord {
	return security.GatewayPendingRecord{Header: security.GatewayPendingHeader{AuthorizationRevision: 1, CreatedAt: time.Now().Unix()}, Kind: "audit", Event: &domain.GatewayEvent{Action: "denied", Reason: "route_unavailable"}}
}

func (f queueFixture) receipt(t *testing.T, wire []byte) []byte {
	t.Helper()
	h, err := security.InspectGatewayPending(wire, f.public)
	if err != nil {
		t.Fatal(err)
	}
	ack, err := security.SignGatewayPendingReceipt(security.GatewayPendingReceipt{AdmissionID: f.binding.AdmissionID, RuntimeID: f.binding.RuntimeID, Sequence: h.Sequence, RecordID: h.RecordID, WireHash: security.GatewayPendingHash(wire)}, f.confirmation)
	if err != nil {
		t.Fatal(err)
	}
	return ack
}

func TestQueueDurabilityACKLossCapacityAndRecovery(t *testing.T) {
	f := newQueueFixture(t)
	q := f.create(t)
	for range f.limits.MaxRecords {
		if q.Append(pendingAudit()) != nil {
			t.Fatal("append failed")
		}
	}
	first, err := q.Next()
	if err != nil {
		t.Fatal(err)
	}
	if q.Append(pendingAudit()) != ErrQueue {
		t.Fatal("unacknowledged record discarded for space")
	}
	again, err := q.Next()
	if err != nil || !bytes.Equal(first, again) {
		t.Fatal("ACK loss changed pending ciphertext")
	}
	if _, err := Create(f.path, f.binding, f.recipient, f.source, f.confirmation.Public().(ed25519.PublicKey), f.limits); err != ErrQueue {
		t.Fatal("second owner admitted")
	}
	ack := f.receipt(t, first)
	for range 2 {
		if q.Acknowledge(ack) != nil {
			t.Fatal("ACK not exact/idempotent")
		}
	}
	if q.Append(pendingAudit()) != nil {
		t.Fatal("confirmed space not reusable")
	}
	if q.Close() != nil {
		t.Fatal("close")
	}
	q = f.recover(t)
	if q.Append(pendingAudit()) != ErrQueue {
		t.Fatal("restart resumed old owner")
	}
	for {
		wire, err := q.Next()
		if err != nil {
			t.Fatal(err)
		}
		if wire == nil {
			break
		}
		if q.Acknowledge(f.receipt(t, wire)) != nil {
			t.Fatal("drain")
		}
	}
	sequence, hash, err := q.Tail()
	if err != nil || sequence != 5 || len(hash) != 64 {
		t.Fatal("lost drained tail")
	}
	if q.Close() != nil {
		t.Fatal("close")
	}
	q = f.recover(t)
	seq, gotHash, err := q.Tail()
	if err != nil || seq != sequence || gotHash != hash {
		t.Fatal("anchor lost after full drain")
	}
}

func TestQueueRejectsWrongAcknowledgementsAndBinding(t *testing.T) {
	f := newQueueFixture(t)
	q := f.create(t)
	if q.Append(pendingAudit()) != nil {
		t.Fatal("append")
	}
	wire, _ := q.Next()
	ack := f.receipt(t, wire)
	r, err := security.VerifyGatewayPendingReceipt(ack, f.confirmation.Public().(ed25519.PublicKey))
	if err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*security.GatewayPendingReceipt){
		func(r *security.GatewayPendingReceipt) { r.RuntimeID = uuid.Must(uuid.NewV7()) },
		func(r *security.GatewayPendingReceipt) { r.RecordID = uuid.Must(uuid.NewV7()) },
		func(r *security.GatewayPendingReceipt) { r.Sequence++ },
		func(r *security.GatewayPendingReceipt) { r.WireHash = strings.Repeat("b", 64) },
	} {
		bad := r
		mutate(&bad)
		signed, _ := security.SignGatewayPendingReceipt(bad, f.confirmation)
		if q.Acknowledge(signed) != ErrQueue {
			t.Fatal("foreign/inexact ACK deleted a record")
		}
	}
	forged, _ := security.SignGatewayPendingReceipt(r, f.source)
	if q.Acknowledge(forged) != ErrQueue {
		t.Fatal("source can acknowledge its own record")
	}
	bad := pendingAudit()
	bad.Header.Binding = f.binding
	bad.Header.Binding.HostID = uuid.Must(uuid.NewV7())
	if q.Append(bad) != ErrQueue {
		t.Fatal("cross-bound adapter accepted")
	}
	if _, _, err = q.Tail(); err != ErrQueue {
		t.Fatal("active queue claimed drained")
	}
	if q.Acknowledge(ack) != nil {
		t.Fatal("valid ACK")
	}
	q.Freeze()
	if q.Append(pendingAudit()) != ErrQueue {
		t.Fatal("frozen queue accepted")
	}
	if seq, _, err := q.Tail(); err != nil || seq != 1 {
		t.Fatal("tail")
	}
}

func TestQueueByteBoundAndNoPlaintextOnDisk(t *testing.T) {
	f := newQueueFixture(t)
	f.limits.MaxBytes = security.MaxGatewayPendingSize + reservedBytes
	q := f.create(t)
	r := pendingAudit()
	r.Kind = "refresh"
	r.Event = nil
	r.ExpectedSecretRevision = 1
	r.Config = bytes.Repeat([]byte("secret-canary"), 18000)
	if q.Append(r) != nil {
		t.Fatal("first refresh")
	}
	if q.Append(r) != ErrQueue {
		t.Fatal("byte bound exceeded")
	}
	entries, _ := os.ReadDir(f.path)
	var total int64
	for _, entry := range entries {
		raw, err := os.ReadFile(filepath.Join(f.path, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(raw, []byte("secret-canary")) {
			t.Fatal("plaintext on disk")
		}
		total += int64(len(raw))
	}
	if total > f.limits.MaxBytes {
		t.Fatal("physical file bytes exceed bound")
	}
}

func TestQueueUncertainWritesFailClosedAndRecoverWithoutResuming(t *testing.T) {
	for _, stage := range []string{"file", "directory"} {
		t.Run(stage, func(t *testing.T) {
			f := newQueueFixture(t)
			q := f.create(t)
			q.syncFile = func(file *os.File) error {
				if (stage == "directory" && file == q.dir) || (stage == "file" && file != q.dir) {
					return errors.New("disk-secret-canary")
				}
				return file.Sync()
			}
			if err := q.Append(pendingAudit()); err != ErrQueue || strings.Contains(err.Error(), "canary") {
				t.Fatal("uncertain write accepted/echoed")
			}
			if q.Append(pendingAudit()) != ErrQueue {
				t.Fatal("uncertain owner continued")
			}
			_ = q.Close()
			if stage == "directory" {
				q = f.recover(t)
				wire, err := q.Next()
				if err != nil || wire == nil {
					t.Fatal("visible encrypted record lost")
				}
			} else if _, err := Recover(f.path, f.binding, f.recipient, f.public, f.confirmation.Public().(ed25519.PublicKey), f.limits); err != ErrQueue {
				t.Fatal("partial temporary accepted")
			}
		})
	}
}

func TestQueueRecoveryRejectsCorruptionAndMissingPrefix(t *testing.T) {
	for _, mode := range []string{"corrupt", "gap", "permissions", "symlink", "identity"} {
		t.Run(mode, func(t *testing.T) {
			f := newQueueFixture(t)
			q := f.create(t)
			for range 2 {
				if q.Append(pendingAudit()) != nil {
					t.Fatal("append")
				}
			}
			_ = q.Close()
			path := filepath.Join(f.path, recordName(1))
			switch mode {
			case "corrupt":
				raw, _ := os.ReadFile(path)
				raw[len(raw)-1] ^= 1
				if os.WriteFile(path, raw, 0600) != nil {
					t.Fatal("mutate")
				}
			case "gap":
				if os.Remove(path) != nil {
					t.Fatal("remove")
				}
			case "permissions":
				if os.Chmod(path, 0644) != nil {
					t.Fatal("chmod")
				}
			case "symlink":
				if os.Remove(path) != nil || os.Symlink(filepath.Join(f.path, recordName(2)), path) != nil {
					t.Fatal("symlink")
				}
			case "identity":
				f.binding.RuntimeID = uuid.Must(uuid.NewV7())
			}
			if _, err := Recover(f.path, f.binding, f.recipient, f.public, f.confirmation.Public().(ed25519.PublicKey), f.limits); err != ErrQueue {
				t.Fatal("corrupt recovery admitted")
			}
		})
	}
}

func TestQueueConcurrentAppendMaintainsOneChain(t *testing.T) {
	f := newQueueFixture(t)
	f.limits.MaxRecords = 32
	q := f.create(t)
	var wg sync.WaitGroup
	for range 32 {
		wg.Go(func() {
			if q.Append(pendingAudit()) != nil {
				t.Error("concurrent append")
			}
		})
	}
	wg.Wait()
	_ = q.Close()
	q = f.recover(t)
	for seq := int64(1); seq <= 32; seq++ {
		wire, err := q.Next()
		if err != nil {
			t.Fatal(err)
		}
		h, err := security.InspectGatewayPending(wire, f.public)
		if err != nil || h.Sequence != seq {
			t.Fatal("chain order")
		}
		if q.Acknowledge(f.receipt(t, wire)) != nil {
			t.Fatal("ACK")
		}
	}
}

func TestQueueUncertainACKRetainsRecordUntilVerifiedRecovery(t *testing.T) {
	f := newQueueFixture(t)
	q := f.create(t)
	if q.Append(pendingAudit()) != nil {
		t.Fatal("append")
	}
	wire, err := q.Next()
	if err != nil {
		t.Fatal(err)
	}
	ack := f.receipt(t, wire)
	q.syncFile = func(file *os.File) error {
		if file == q.dir {
			return errors.New("confirmation directory sync failed")
		}
		return file.Sync()
	}
	if q.Acknowledge(ack) != ErrQueue {
		t.Fatal("uncertain ACK accepted")
	}
	if _, err = os.Stat(filepath.Join(f.path, recordName(1))); err != nil {
		t.Fatal("record deleted before durable confirmation")
	}
	_ = q.Close()
	q = f.recover(t)
	if wire, err = q.Next(); err != nil || wire != nil {
		t.Fatal("verified committed leftover not reclaimed")
	}
	if sequence, _, err := q.Tail(); err != nil || sequence != 1 {
		t.Fatal("confirmed tail lost")
	}
}
