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

func TestAuthorityChannelSerialDeliveryAndCancellationJoin(t *testing.T) {
	f := newQueueFixture(t)
	path := replaySocket(t)
	l, err := ListenReplay(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var accepted atomic.Int64
	done := make(chan error, 1)
	go func() {
		done <- ServeAuthorization(ctx, l, uint32(os.Geteuid()), f.binding, f.source, f.confirmation.Public().(ed25519.PublicKey),
			func(_ context.Context, wire []byte) error {
				s, err := security.VerifyGatewayStatement(wire, f.confirmation.Public().(ed25519.PublicKey))
				if err != nil || s.Revision != uint64(accepted.Load()+1) {
					return security.ErrGatewayAuthority
				}
				accepted.Add(1)
				return nil
			}, func(context.Context) error { return nil })
	}()
	for revision := uint64(1); revision <= 3; revision++ {
		statement := security.GatewayStatement{Binding: f.binding, Revision: revision, IssuedAt: time.Now().Unix() - 1,
			ExpiresAt: time.Now().Add(time.Minute).Unix()}
		if revision == 3 {
			statement.Revoked, statement.ExpiresAt = true, 0
		}
		wire, err := security.SignGatewayStatement(statement, f.confirmation)
		if err != nil || SendAuthorization(ctx, path, uint32(os.Geteuid()), f.binding, f.public,
			func(context.Context) ([]byte, error) { return wire, nil }, func(context.Context) error { return nil }) != nil {
			t.Fatal("serial metadata delivery failed")
		}
	}
	cancel()
	if <-done != nil || accepted.Load() != 3 {
		t.Fatal("authorization listener did not join")
	}
}

func TestAuthoritySenderRejectsFramesAndReceiptsBeforeConfirmation(t *testing.T) {
	for _, failure := range []string{"magic", "version", "runtime", "empty", "oversize", "receipt-nonce", "receipt-hash", "receipt-loss"} {
		t.Run(failure, func(t *testing.T) {
			f := newQueueFixture(t)
			path := replaySocket(t)
			l, err := ListenReplay(path)
			if err != nil { t.Fatal(err) }
			defer l.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			var decided atomic.Bool
			done := make(chan error, 1)
			go func() {
				conn, err := l.AcceptUnix()
				if err != nil { done <- err; return }
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(2*time.Second))
				challenge, err := security.NewGatewayAuthorityChallenge(f.binding)
				if err != nil { done <- err; return }
				proof, err := security.SignGatewayAuthorityChallenge(challenge, f.source)
				if err != nil { done <- err; return }
				var frame bytes.Buffer
				if err := writeChannelFrame(&frame, "RFGA", f.binding.RuntimeID, proof); err != nil { done <- err; return }
				raw := frame.Bytes()
				switch failure {
				case "magic": copy(raw[:4], "RFGM")
				case "version": binary.BigEndian.PutUint32(raw[4:8], 2)
				case "runtime": copy(raw[8:24], make([]byte, 16))
				case "empty": binary.BigEndian.PutUint32(raw[24:28], 0)
				case "oversize": binary.BigEndian.PutUint32(raw[24:28], 2049)
				}
				if _, err = conn.Write(raw); err != nil { done <- err; return }
				if failure == "magic" || failure == "version" || failure == "runtime" || failure == "empty" || failure == "oversize" {
					done <- nil; return
				}
				_, wire, err := readChannelFrame(conn, "RFGA", security.MaxGatewayStatementSize)
				if err != nil { done <- err; return }
				if failure == "receipt-loss" { done <- nil; return }
				hash := security.GatewayPendingHash(wire)
				if failure == "receipt-nonce" { challenge.Nonce[0] ^= 1 }
				if failure == "receipt-hash" { hash = security.GatewayPendingHash([]byte("wrong-wire")) }
				ack, err := security.SignGatewayAuthorityReceipt(security.GatewayAuthorityReceipt{Challenge: challenge, WireHash: hash}, f.source)
				if err == nil { err = writeChannelFrame(conn, "RFGA", f.binding.RuntimeID, ack) }
				done <- err
			}()
			wire, err := security.SignGatewayStatement(security.GatewayStatement{Binding: f.binding, Revision: 1,
				IssuedAt: time.Now().Unix()-1, ExpiresAt: time.Now().Add(time.Minute).Unix()}, f.confirmation)
			if err != nil { t.Fatal(err) }
			err = SendAuthorization(ctx, path, uint32(os.Geteuid()), f.binding, f.public,
				func(context.Context) ([]byte, error) { decided.Store(true); return wire, nil }, func(context.Context) error { return nil })
			if err != ErrChannel || <-done != nil || decided.Load() != (failure == "receipt-nonce" || failure == "receipt-hash" || failure == "receipt-loss") {
				t.Fatal("bad framing reached transaction or invalid receipt confirmed delivery")
			}
		})
	}
}

func TestAuthorityCancellationJoinsBlockedAcceptance(t *testing.T) {
	f := newQueueFixture(t)
	path := replaySocket(t)
	l, err := ListenReplay(path)
	if err != nil { t.Fatal(err) }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- ServeAuthorization(ctx, l, uint32(os.Geteuid()), f.binding, f.source, f.confirmation.Public().(ed25519.PublicKey),
			func(ctx context.Context, _ []byte) error { close(entered); <-ctx.Done(); <-release; return ctx.Err() }, func(context.Context) error { return nil })
	}()
	conn, err := net.Dial("unix", path)
	if err != nil { t.Fatal(err) }
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(2*time.Second))
	if _, _, err := readChannelFrame(conn, "RFGA", 2048); err != nil { t.Fatal(err) }
	wire, err := security.SignGatewayStatement(security.GatewayStatement{Binding: f.binding, Revision: 1,
		IssuedAt: time.Now().Unix()-1, ExpiresAt: time.Now().Add(time.Minute).Unix()}, f.confirmation)
	if err != nil || writeChannelFrame(conn, "RFGA", f.binding.RuntimeID, wire) != nil { t.Fatal("test delivery") }
	select { case <-entered: case <-time.After(2*time.Second): t.Fatal("acceptance not entered") }
	cancel()
	select { case <-done: t.Fatal("listener returned before callback joined"); case <-time.After(30*time.Millisecond): }
	close(release)
	if <-done != nil { t.Fatal("listener did not join callback") }
}

func TestAuthorityAuditFailureStopsListener(t *testing.T) {
	f := newQueueFixture(t)
	path := replaySocket(t)
	l, err := ListenReplay(path)
	if err != nil { t.Fatal(err) }
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- ServeAuthorization(ctx, l, uint32(os.Geteuid()), f.binding, f.source, f.confirmation.Public().(ed25519.PublicKey),
			func(context.Context, []byte) error { return errors.New("private-acceptance-canary") },
			func(context.Context) error { return errors.New("private-audit-canary") })
	}()
	wire, err := security.SignGatewayStatement(security.GatewayStatement{Binding: f.binding, Revision: 1,
		IssuedAt: time.Now().Unix()-1, ExpiresAt: time.Now().Add(time.Minute).Unix()}, f.confirmation)
	if err != nil { t.Fatal(err) }
	if SendAuthorization(ctx, path, uint32(os.Geteuid()), f.binding, f.public,
		func(context.Context) ([]byte, error) { return wire, nil }, func(context.Context) error { return nil }) != ErrChannel || <-done != ErrChannel {
		t.Fatal("audit failure left listener available or exposed raw error")
	}
}

func TestAuthorityChannelRejectsPeerProofBindingAndCallback(t *testing.T) {
	for _, failure := range []string{"receiver-uid", "sender-uid", "source", "binding", "center-pin", "callback", "cancellation"} {
		t.Run(failure, func(t *testing.T) {
			f := newQueueFixture(t)
			path := replaySocket(t)
			l, err := ListenReplay(path)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			uid, peer := uint32(os.Geteuid()), uint32(os.Geteuid())
			if failure == "receiver-uid" {
				peer++
			}
			center := f.confirmation.Public().(ed25519.PublicKey)
			if failure == "center-pin" {
				center = f.public
			}
			var applied, decided atomic.Bool
			done := make(chan error, 1)
			go func() {
				done <- ServeAuthorization(ctx, l, peer, f.binding, f.source, center, func(ctx context.Context, _ []byte) error {
					applied.Store(true)
					if failure == "cancellation" {
						cancel()
						<-ctx.Done()
					}
					return errors.New("private-callback-canary")
				}, func(context.Context) error { return nil })
			}()
			binding, source := f.binding, f.public
			if failure == "binding" {
				binding.HostID = uuid.Must(uuid.NewV7())
			}
			if failure == "source" {
				source = f.confirmation.Public().(ed25519.PublicKey)
			}
			if failure == "sender-uid" {
				uid++
			}
			request := security.GatewayStatement{Binding: f.binding, Revision: 1, IssuedAt: time.Now().Unix() - 1, ExpiresAt: time.Now().Add(time.Minute).Unix()}
			wire, err := security.SignGatewayStatement(request, f.confirmation)
			if err != nil {
				t.Fatal(err)
			}
			err = SendAuthorization(ctx, path, uid, binding, source, func(context.Context) ([]byte, error) {
				decided.Store(true)
				return wire, nil
			}, func(context.Context) error { return nil })
			cancel()
			if err != ErrChannel || <-done != nil || applied.Load() != (failure == "callback" || failure == "cancellation") {
				t.Fatal("forbidden authority reached callback or shutdown failed")
			}
			if failure != "center-pin" && failure != "callback" && failure != "cancellation" && decided.Load() {
				t.Fatal("unauthenticated peer/proof reached central transaction")
			}
		})
	}
}
