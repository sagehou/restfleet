package gatewaypending

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/binary"
	"errors"
	"net"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sagehou/restfleet/internal/security"
)

func materialDelivery(t *testing.T, f queueFixture, challenge []byte) []byte {
	t.Helper()
	c, err := security.VerifyGatewayMaterialChallenge(challenge, f.public, f.binding)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	statement, err := security.SignGatewayStatement(security.GatewayStatement{Binding: f.binding, Revision: 1, IssuedAt: now - 1, ExpiresAt: now + 60}, f.confirmation)
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte("material-secret-canary")
	wire, err := security.SealGatewayMaterial(security.GatewayMaterial{Challenge: c, Source: f.public, PendingRecipient: f.recipient, Statement: statement,
		AdmissionCreatedAt: now - 2, AdmissionExpiresAt: now + 120, SecretRevision: 1, Remote: "encrypted", ConfigHash: security.GatewayPendingHash(raw), Config: raw}, f.confirmation)
	if err != nil {
		t.Fatal(err)
	}
	return wire
}

func TestMaterialChannelAuthenticatesAndClearsBorrowedConfig(t *testing.T) {
	f := newQueueFixture(t)
	receiver, err := NewMaterialReceiver(f.binding, f.source, f.confirmation.Public().(ed25519.PublicKey))
	if err != nil {
		t.Fatal(err)
	}
	path := replaySocket(t)
	listener, err := ListenReplay(path)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	var borrowed []byte
	var rollback atomic.Bool
	go func() {
		done <- receiver.Receive(context.Background(), listener, uint32(os.Geteuid()), func(_ context.Context, m security.GatewayMaterial) (func(), error) {
			if !bytes.Equal(m.Config, []byte("material-secret-canary")) {
				t.Error("material changed")
			}
			borrowed = m.Config
			return func() { rollback.Store(true) }, nil
		}, func(context.Context) error { return nil })
	}()
	err = SendMaterial(context.Background(), path, uint32(os.Geteuid()), f.binding, f.public, func(_ context.Context, challenge []byte) ([]byte, error) {
		return materialDelivery(t, f, challenge), nil
	}, func(context.Context) error { return nil })
	if err != nil || <-done != nil || rollback.Load() || len(bytes.Trim(borrowed, "\x00")) != 0 {
		t.Fatal("delivery/clearing failed")
	}
	if receiver.Receive(context.Background(), listener, uint32(os.Geteuid()), func(context.Context, security.GatewayMaterial) (func(), error) {
		t.Error("second install")
		return nil, nil
	}, func(context.Context) error { return nil }) != ErrChannel {
		t.Fatal("receiver reused")
	}
	if len(bytes.Trim(receiver.source, "\x00")) != 0 || receiver.recipient != ([32]byte{}) {
		t.Fatal("ephemeral private keys survived")
	}
}

func TestMaterialReceiverCloseConsumesAndJoins(t *testing.T) {
	for _, state := range []string{"unused", "accepting", "invalid"} {
		t.Run(state, func(t *testing.T) {
			f := newQueueFixture(t)
			r, err := NewMaterialReceiver(f.binding, f.source, f.confirmation.Public().(ed25519.PublicKey))
			if err != nil {
				t.Fatal(err)
			}
			install := func(context.Context, security.GatewayMaterial) (func(), error) {
				t.Error("unexpected install")
				return nil, nil
			}
			denied := func(context.Context) error { return nil }
			if state == "accepting" {
				listener, err := ListenReplay(replaySocket(t))
				if err != nil {
					t.Fatal(err)
				}
				done := make(chan error, 1)
				go func() { done <- r.Receive(context.Background(), listener, uint32(os.Geteuid()), install, denied) }()
				deadline := time.Now().Add(time.Second)
				for {
					r.mu.Lock()
					used := r.used
					r.mu.Unlock()
					if used {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("receiver did not start")
					}
					time.Sleep(time.Millisecond)
				}
				r.Close()
				if <-done != ErrChannel {
					t.Fatal("canceled receiver accepted")
				}
			} else if state == "invalid" && r.Receive(context.Background(), nil, 0, install, denied) != ErrChannel {
				t.Fatal("invalid listener accepted")
			}
			r.Close()
			r.Close()
			if len(bytes.Trim(r.source, "\x00")) != 0 || r.recipient != ([32]byte{}) ||
				r.Receive(context.Background(), nil, 0, install, denied) != ErrChannel {
				t.Fatal("closed receiver retained keys or was reusable")
			}
		})
	}
}

func TestMaterialChannelRejectsPeerProofPayloadAndInstall(t *testing.T) {
	for _, failure := range []string{"receiver-uid", "sender-uid", "source", "binding", "ciphertext", "install", "deadline"} {
		t.Run(failure, func(t *testing.T) {
			f := newQueueFixture(t)
			receiver, _ := NewMaterialReceiver(f.binding, f.source, f.confirmation.Public().(ed25519.PublicKey))
			path := replaySocket(t)
			listener, err := ListenReplay(path)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			uid := uint32(os.Geteuid())
			receiverUID, senderUID := uid, uid
			if failure == "receiver-uid" {
				receiverUID++
			}
			if failure == "sender-uid" {
				senderUID++
			}
			var installed, rolled, called atomic.Bool
			done := make(chan error, 1)
			go func() {
				done <- receiver.Receive(ctx, listener, receiverUID, func(ctx context.Context, _ security.GatewayMaterial) (func(), error) {
					installed.Store(true)
					if failure == "deadline" {
						<-ctx.Done()
					}
					if failure == "install" {
						return func() { rolled.Store(true) }, errors.New("secret-canary")
					}
					return func() { rolled.Store(true) }, nil
				}, func(context.Context) error { return nil })
			}()
			binding := f.binding
			public := f.public
			if failure == "source" {
				public = f.confirmation.Public().(ed25519.PublicKey)
			}
			if failure == "binding" {
				binding.HostID = uuid.Must(uuid.NewV7())
			}
			err = SendMaterial(ctx, path, senderUID, binding, public, func(_ context.Context, challenge []byte) ([]byte, error) {
				called.Store(true)
				wire := materialDelivery(t, f, challenge)
				if failure == "ciphertext" {
					wire[len(wire)-1] ^= 1
				}
				return wire, nil
			}, func(context.Context) error { return nil })
			if err != ErrChannel || <-done != ErrChannel {
				t.Fatal("unsafe delivery accepted")
			}
			shouldInstall := failure == "install" || failure == "deadline"
			if installed.Load() != shouldInstall || rolled.Load() != shouldInstall {
				t.Fatal("install/rollback boundary")
			}
			if !shouldInstall && failure != "ciphertext" && called.Load() {
				t.Fatal("unauthenticated proof reached central delivery")
			}
		})
	}
}

func TestMaterialChannelVersionBoundsAndReceiptLoss(t *testing.T) {
	for _, failure := range []string{"version", "runtime", "size", "replay-magic", "receipt-loss"} {
		t.Run(failure, func(t *testing.T) {
			f := newQueueFixture(t)
			receiver, _ := NewMaterialReceiver(f.binding, f.source, f.confirmation.Public().(ed25519.PublicKey))
			path := replaySocket(t)
			listener, err := ListenReplay(path)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			var installed, rolled atomic.Bool
			entered, release := make(chan struct{}), make(chan struct{})
			done := make(chan error, 1)
			go func() {
				done <- receiver.Receive(ctx, listener, uint32(os.Geteuid()), func(context.Context, security.GatewayMaterial) (func(), error) {
					installed.Store(true)
					close(entered)
					<-release
					return func() { rolled.Store(true) }, nil
				}, func(context.Context) error { return nil })
			}()
			conn, err := net.Dial("unix", path)
			if err != nil {
				t.Fatal(err)
			}
			_ = conn.SetDeadline(time.Now().Add(time.Second))
			_, challenge, err := readChannelFrame(conn, "RFGM", 2048)
			if err != nil {
				t.Fatal(err)
			}
			wire := materialDelivery(t, f, challenge)
			if failure == "receipt-loss" {
				if writeChannelFrame(conn, "RFGM", f.binding.RuntimeID, wire) != nil {
					t.Fatal("write")
				}
				select {
				case <-entered:
				case <-ctx.Done():
					t.Fatal("install did not start")
				}
				_ = conn.Close()
				close(release)
			} else {
				var buf bytes.Buffer
				_ = writeChannelFrame(&buf, "RFGM", f.binding.RuntimeID, wire)
				raw := buf.Bytes()
				switch failure {
				case "version":
					binary.BigEndian.PutUint32(raw[4:8], 2)
				case "runtime":
					other := uuid.Must(uuid.NewV7())
					copy(raw[8:24], other[:])
				case "size":
					binary.BigEndian.PutUint32(raw[24:], 1<<30)
				case "replay-magic":
					copy(raw[:4], "RFGR")
				}
				_, _ = conn.Write(raw)
				_ = conn.Close()
				close(release)
			}
			if <-done != ErrChannel || installed.Load() != (failure == "receipt-loss") || rolled.Load() != (failure == "receipt-loss") {
				t.Fatal("framing or lost receipt boundary")
			}
		})
	}
}

func TestMaterialStartupWaitDoesNotConsumeExchangeDeadline(t *testing.T) {
	f := newQueueFixture(t)
	r, err := NewMaterialReceiver(f.binding, f.source, f.confirmation.Public().(ed25519.PublicKey))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	path := replaySocket(t)
	l, err := ListenReplay(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- r.ReceiveWaiting(ctx, l, uint32(os.Geteuid()), 10*time.Second,
			func(work context.Context, _ security.GatewayMaterial) (func(), error) {
				deadline, ok := work.Deadline()
				if remaining := time.Until(deadline); !ok || remaining <= time.Second || remaining > 5*time.Second {
					t.Error("startup waiting consumed or extended exchange deadline")
				}
				return func() {}, nil
			}, func(context.Context) error { t.Error("successful startup denied"); return nil })
	}()
	// Reproduce a central coordinator starting after the old total 5s budget.
	select {
	case err := <-done:
		t.Fatalf("receiver stopped while waiting: %v", err)
	case <-time.After(5100 * time.Millisecond):
	}
	if err := SendMaterial(ctx, path, uint32(os.Geteuid()), f.binding, f.public,
		func(_ context.Context, challenge []byte) ([]byte, error) {
			return materialDelivery(t, f, challenge), nil
		},
		func(context.Context) error { return nil }); err != nil || <-done != nil {
		t.Fatal("bounded startup wait did not preserve authenticated exchange")
	}
}

func TestMaterialStartupWaitFailureConsumesAndClearsReceiver(t *testing.T) {
	for _, wait := range []time.Duration{0, -time.Second, 5*time.Minute + 1, 20 * time.Millisecond} {
		t.Run(wait.String(), func(t *testing.T) {
			f := newQueueFixture(t)
			r, err := NewMaterialReceiver(f.binding, f.source, f.confirmation.Public().(ed25519.PublicKey))
			if err != nil {
				t.Fatal(err)
			}
			l, err := ListenReplay(replaySocket(t))
			if err != nil {
				t.Fatal(err)
			}
			err = r.ReceiveWaiting(context.Background(), l, uint32(os.Geteuid()), wait,
				func(context.Context, security.GatewayMaterial) (func(), error) {
					t.Error("unexpected install")
					return nil, nil
				},
				func(audit context.Context) error {
					deadline, ok := audit.Deadline()
					if audit.Err() != nil || !ok || time.Until(deadline) > 3*time.Second {
						t.Error("expired wait canceled or unbounded denial audit")
					}
					return nil
				})
			r.Close()
			if err != ErrChannel || len(bytes.Trim(r.source, "\x00")) != 0 || r.recipient != ([32]byte{}) {
				t.Fatal("failed wait left receiver available or retained private keys")
			}
			if _, err := l.AcceptUnix(); err == nil {
				t.Fatal("failed wait retained listener")
			}
		})
	}
}

func TestMaterialStartupWaitKeepsFiveSecondExchangeLimit(t *testing.T) {
	f := newQueueFixture(t)
	r, err := NewMaterialReceiver(f.binding, f.source, f.confirmation.Public().(ed25519.PublicKey))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	path := replaySocket(t)
	l, err := ListenReplay(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- r.ReceiveWaiting(ctx, l, uint32(os.Geteuid()), 5*time.Minute,
			func(context.Context, security.GatewayMaterial) (func(), error) {
				t.Error("stalled peer installed")
				return nil, nil
			},
			func(audit context.Context) error {
				if audit.Err() != nil {
					t.Error("exchange deadline canceled audit")
				}
				return nil
			})
	}()
	conn, err := net.DialTimeout("unix", path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	if _, _, err := readChannelFrame(conn, "RFGM", 2048); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != ErrChannel {
			t.Fatal("stalled exchange accepted")
		}
	case <-time.After(6 * time.Second):
		t.Fatal("startup wait extended exchange beyond five seconds")
	}
}

func TestMaterialReceiverCloseJoinsIndependentDenialAudit(t *testing.T) {
	f := newQueueFixture(t)
	r, err := NewMaterialReceiver(f.binding, f.source, f.confirmation.Public().(ed25519.PublicKey))
	if err != nil {
		t.Fatal(err)
	}
	path := replaySocket(t)
	l, err := ListenReplay(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- r.ReceiveWaiting(ctx, l, uint32(os.Geteuid()), 5*time.Minute,
			func(context.Context, security.GatewayMaterial) (func(), error) {
				t.Error("unexpected install")
				return nil, nil
			},
			func(audit context.Context) error {
				deadline, ok := audit.Deadline()
				if audit.Err() != nil || !ok || time.Until(deadline) > 3*time.Second {
					t.Error("shutdown canceled or unbounded audit")
				}
				close(entered)
				select {
				case <-release:
				case <-audit.Done():
					return audit.Err()
				}
				return nil
			})
	}()
	conn, err := net.DialTimeout("unix", path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	if _, _, err := readChannelFrame(conn, "RFGM", 2048); err != nil {
		t.Fatal(err)
	}
	closed := make(chan struct{})
	go func() { r.Close(); close(closed) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("Close did not audit observed failure")
	}
	select {
	case <-closed:
		t.Fatal("Close returned before audit joined")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if <-done != ErrChannel {
		t.Fatal("Close accepted incomplete exchange")
	}
	<-closed
}

func TestMaterialSenderCancellationPreservesDenialAudit(t *testing.T) {
	f := newQueueFixture(t)
	path := replaySocket(t)
	l, err := ListenReplay(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		conn, err := l.AcceptUnix()
		if err != nil {
			done <- err
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
		challenge, err := security.NewGatewayMaterialChallenge(f.binding, f.recipient)
		if err != nil {
			done <- err
			return
		}
		proof, err := security.SignGatewayMaterialChallenge(challenge, f.source)
		if err == nil {
			err = writeChannelFrame(conn, "RFGM", f.binding.RuntimeID, proof)
		}
		done <- err
	}()
	var audited bool
	err = SendMaterial(ctx, path, uint32(os.Geteuid()), f.binding, f.public,
		func(context.Context, []byte) ([]byte, error) {
			cancel()
			return nil, errors.New("private-delivery-canary")
		},
		func(audit context.Context) error {
			deadline, ok := audit.Deadline()
			if audit.Err() != nil || !ok || time.Until(deadline) > 3*time.Second {
				t.Error("sender cancellation lost denial audit")
			}
			audited = true
			return nil
		})
	if err != ErrChannel || !audited || <-done != nil {
		t.Fatal("sender failed without independent denial audit or exposed private error")
	}
}
