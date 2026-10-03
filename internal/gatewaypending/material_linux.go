package gatewaypending

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"os"
	"sync"
	"time"

	"github.com/sagehou/restfleet/internal/security"
	"golang.org/x/crypto/nacl/box"
)

// MaterialReceiver accepts ONE protected center-to-Gateway initialization, not
// backup traffic, authority renewal or a process takeover. Pins/source private
// key MUST come from trusted local provisioning, never a request. Recipient
// private key is ephemeral and remains local. Unknown/malformed peers consume
// this one attempt and retain the central fence for explicit coordination.
type MaterialReceiver struct {
	mu        sync.Mutex
	used      bool
	cancel    context.CancelFunc
	done      chan struct{}
	binding   security.GatewayAuthorizationBinding
	source    ed25519.PrivateKey
	central   ed25519.PublicKey
	challenge security.GatewayMaterialChallenge
	recipient [32]byte
}

func NewMaterialReceiver(binding security.GatewayAuthorizationBinding, source ed25519.PrivateKey, central ed25519.PublicKey) (*MaterialReceiver, error) {
	if binding.Validate() != nil || len(source) != 64 || len(central) != 32 {
		return nil, ErrChannel
	}
	public, private, err := box.GenerateKey(rand.Reader)
	if err != nil {
		return nil, ErrChannel
	}
	defer clear(private[:])
	challenge, err := security.NewGatewayMaterialChallenge(binding, *public)
	if err != nil {
		return nil, ErrChannel
	}
	if _, err = security.SignGatewayMaterialChallenge(challenge, source); err != nil {
		return nil, ErrChannel
	}
	return &MaterialReceiver{binding: binding, source: append(ed25519.PrivateKey(nil), source...), central: append(ed25519.PublicKey(nil), central...), challenge: challenge, recipient: *private, done: make(chan struct{})}, nil
}

// Close consumes an unused receiver or cancels and joins its active exchange.
// It clears local private keys, without proving central fence release. Do not
// call it from install/rollback/denied callbacks, which it waits to finish.
func (r *MaterialReceiver) Close() {
	r.mu.Lock()
	if !r.used {
		r.used = true
		clear(r.source)
		clear(r.recipient[:])
		close(r.done)
	} else if r.cancel != nil {
		r.cancel()
	}
	r.mu.Unlock()
	<-r.done
}

// Receive owns and closes listener. The caller MUST use a freshly bound socket
// in a service-owned 0700 directory (ListenReplay enforces the same local path
// policy). This protocol has separate RFGM magic and cannot parse RFGR frames.
// install MUST honor ctx and return a rollback that cancels/joins any owner if
// the receipt cannot be sent. Config is cleared after the borrowed callback.
// Successful receipt means initialization only, never READY, Agent ACK or cleanup.
func (r *MaterialReceiver) Receive(ctx context.Context, listener *net.UnixListener, centerUID uint32,
	install func(context.Context, security.GatewayMaterial) (func(), error), denied func(context.Context) error,
) (failure error) {
	r.mu.Lock()
	if r.used {
		r.mu.Unlock()
		return ErrChannel
	}
	r.used = true
	work, cancel := context.WithTimeout(ctx, 5*time.Second)
	r.cancel = cancel
	r.mu.Unlock()
	defer close(r.done)
	defer clear(r.source)
	defer clear(r.recipient[:])
	defer cancel()
	if listener != nil {
		defer listener.Close()
	}
	if listener == nil || install == nil || denied == nil || !privateReplayPath(listener.Addr().String(), uint32(os.Geteuid()), true) {
		return ErrChannel
	}
	defer func() {
		if failure != nil {
			_ = denied(work)
		}
	}()
	stopListener := context.AfterFunc(work, func() { _ = listener.Close() })
	defer stopListener()
	conn, err := listener.AcceptUnix()
	if err != nil {
		return ErrChannel
	}
	defer conn.Close()
	stop := context.AfterFunc(work, func() { _ = conn.Close() })
	defer stop()
	deadline, _ := work.Deadline()
	_ = conn.SetDeadline(deadline)
	if !peerUID(conn, centerUID) {
		return ErrChannel
	}
	challenge, err := security.SignGatewayMaterialChallenge(r.challenge, r.source)
	if err != nil || writeChannelFrame(conn, "RFGM", r.binding.RuntimeID, challenge) != nil {
		return ErrChannel
	}
	runtime, wire, err := readChannelFrame(conn, "RFGM", security.MaxGatewayMaterialSize)
	if err != nil || runtime != r.binding.RuntimeID || work.Err() != nil {
		return ErrChannel
	}
	material, err := security.OpenGatewayMaterial(wire, r.challenge, r.source.Public().(ed25519.PublicKey), r.central, r.recipient[:])
	if err != nil {
		return ErrChannel
	}
	defer clear(material.Config)
	statement, err := security.VerifyGatewayStatement(material.Statement, r.central)
	if err != nil || statement.IssuedAt > time.Now().Unix() || statement.ExpiresAt <= time.Now().Unix() {
		return ErrChannel
	}
	rollback, err := install(work, material)
	if rollback != nil {
		defer func() {
			if failure != nil {
				rollback()
			}
		}()
	}
	if err != nil || rollback == nil || work.Err() != nil || statement.ExpiresAt <= time.Now().Unix() {
		return ErrChannel
	}
	ack, err := security.SignGatewayMaterialReceipt(security.GatewayMaterialReceipt{Challenge: r.challenge, WireHash: security.GatewayPendingHash(wire)}, r.source)
	if err != nil || writeChannelFrame(conn, "RFGM", r.binding.RuntimeID, ack) != nil || work.Err() != nil {
		return ErrChannel
	}
	return nil
}

// SendMaterial verifies proof of the pre-registered Gateway source BEFORE
// calling central delivery. Pins/binding/path must come from the trusted
// coordinator. The callback rechecks current DB state and returns ciphertext.
// Failure after sending is uncertain initialization, never proof of cleanup;
// do not resend material to reset an owner or release its fence.
func SendMaterial(ctx context.Context, path string, gatewayUID uint32, binding security.GatewayAuthorizationBinding, source ed25519.PublicKey,
	deliver func(context.Context, []byte) ([]byte, error), denied func(context.Context) error,
) (failure error) {
	if binding.Validate() != nil || len(source) != 32 || deliver == nil || denied == nil || !privateReplayPath(path, gatewayUID, true) {
		return ErrChannel
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	defer func() {
		if failure != nil {
			_ = denied(ctx)
		}
	}()
	dialer := net.Dialer{}
	connection, err := dialer.DialContext(ctx, "unix", path)
	if err != nil {
		return ErrChannel
	}
	defer connection.Close()
	conn, ok := connection.(*net.UnixConn)
	if !ok || !peerUID(conn, gatewayUID) {
		return ErrChannel
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	deadline, _ := ctx.Deadline()
	_ = conn.SetDeadline(deadline)
	runtime, challengeWire, err := readChannelFrame(conn, "RFGM", 2048)
	if err != nil || runtime != binding.RuntimeID {
		return ErrChannel
	}
	challenge, err := security.VerifyGatewayMaterialChallenge(challengeWire, source, binding)
	if err != nil || ctx.Err() != nil {
		return ErrChannel
	}
	wire, err := deliver(ctx, challengeWire)
	if err != nil || len(wire) == 0 || len(wire) > security.MaxGatewayMaterialSize || ctx.Err() != nil || writeChannelFrame(conn, "RFGM", runtime, wire) != nil {
		return ErrChannel
	}
	returned, ack, err := readChannelFrame(conn, "RFGM", 2048)
	if err != nil || returned != runtime || ctx.Err() != nil || security.VerifyGatewayMaterialReceipt(ack, challenge, security.GatewayPendingHash(wire), source) != nil {
		return ErrChannel
	}
	return nil
}
