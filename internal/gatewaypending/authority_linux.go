package gatewaypending

import (
	"context"
	"crypto/ed25519"
	"net"
	"os"
	"time"

	"github.com/sagehou/restfleet/internal/security"
)

// ServeAuthorization owns a new protected Unix listener and serializes bounded
// metadata-only deliveries for ONE immutable binding. It copies/clears source;
// callers provision keys locally and must not log wire or raw callback errors.
// Cancellation closes the active connection and joins its callback. Transport
// failure never revokes, renews or rolls back an already accepted statement.
func ServeAuthorization(ctx context.Context, listener *net.UnixListener, centerUID uint32,
	binding security.GatewayAuthorizationBinding, source ed25519.PrivateKey, central ed25519.PublicKey,
	accept func(context.Context, []byte) error, denied func(context.Context) error, sharedGroup ...uint32,
) error {
	if listener != nil {
		defer listener.Close()
	}
	challenge, err := security.NewGatewayAuthorityChallenge(binding)
	if err != nil || listener == nil || accept == nil || denied == nil || len(central) != 32 ||
		!privateReplayPath(listener.Addr().String(), uint32(os.Geteuid()), true, sharedGroup...) {
		return ErrChannel
	}
	source = append(ed25519.PrivateKey(nil), source...)
	defer clear(source)
	central = append(ed25519.PublicKey(nil), central...)
	if _, err = security.SignGatewayAuthorityChallenge(challenge, source); err != nil {
		return ErrChannel
	}
	stop := context.AfterFunc(ctx, func() { _ = listener.Close() })
	defer stop()
	for {
		conn, err := listener.AcceptUnix()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return ErrChannel
		}
		request, cancel := context.WithTimeout(ctx, 5*time.Second)
		err = receiveAuthorization(request, conn, centerUID, binding, source, central, accept)
		cancel()
		_ = conn.Close()
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			// A caller may cancel immediately after transport failure. Preserve
			// this observed rejection through the bounded audit and join it.
			audit, finish := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
			err = denied(audit)
			finish()
			if err != nil {
				return ErrChannel
			}
		}
	}
}

func receiveAuthorization(ctx context.Context, conn *net.UnixConn, centerUID uint32,
	binding security.GatewayAuthorizationBinding, source ed25519.PrivateKey, central ed25519.PublicKey,
	accept func(context.Context, []byte) error,
) error {
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	deadline, _ := ctx.Deadline()
	_ = conn.SetDeadline(deadline)
	if !peerUID(conn, centerUID) {
		return ErrChannel
	}
	challenge, err := security.NewGatewayAuthorityChallenge(binding)
	if err != nil {
		return ErrChannel
	}
	proof, err := security.SignGatewayAuthorityChallenge(challenge, source)
	if err != nil || writeChannelFrame(conn, "RFGA", binding.RuntimeID, proof) != nil {
		return ErrChannel
	}
	runtime, wire, err := readChannelFrame(conn, "RFGA", security.MaxGatewayStatementSize)
	if err != nil || runtime != binding.RuntimeID || ctx.Err() != nil {
		return ErrChannel
	}
	statement, err := security.VerifyGatewayStatement(wire, central)
	if err != nil || statement.Binding != binding || accept(ctx, wire) != nil || ctx.Err() != nil {
		return ErrChannel
	}
	ack, err := security.SignGatewayAuthorityReceipt(security.GatewayAuthorityReceipt{Challenge: challenge, WireHash: security.GatewayPendingHash(wire)}, source)
	if err != nil || writeChannelFrame(conn, "RFGA", runtime, ack) != nil || ctx.Err() != nil {
		return ErrChannel
	}
	return nil
}

// SendAuthorization authenticates source/peer BEFORE calling the central
// transaction. A lost receipt is uncertain acceptance: never undo the decision,
// deliver material again or release a fence. Exact latest-decision replay may
// be attempted explicitly; it must not issue a fresh grant or extend its life.
func SendAuthorization(ctx context.Context, path string, gatewayUID uint32, binding security.GatewayAuthorizationBinding,
	source ed25519.PublicKey, decide func(context.Context) ([]byte, error), denied func(context.Context) error, sharedGroup ...uint32,
) (failure error) {
	if binding.Validate() != nil || len(source) != 32 || decide == nil || denied == nil || !privateReplayPath(path, gatewayUID, true, sharedGroup...) {
		return ErrChannel
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	defer func() {
		if failure != nil {
			audit, finish := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
			defer finish()
			_ = denied(audit)
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
	runtime, proof, err := readChannelFrame(conn, "RFGA", 2048)
	if err != nil || runtime != binding.RuntimeID {
		return ErrChannel
	}
	challenge, err := security.VerifyGatewayAuthorityChallenge(proof, source, binding)
	if err != nil || ctx.Err() != nil {
		return ErrChannel
	}
	wire, err := decide(ctx)
	if err != nil || len(wire) == 0 || len(wire) > security.MaxGatewayStatementSize || ctx.Err() != nil ||
		writeChannelFrame(conn, "RFGA", runtime, wire) != nil {
		return ErrChannel
	}
	returned, ack, err := readChannelFrame(conn, "RFGA", 2048)
	if err != nil || returned != runtime || ctx.Err() != nil || security.VerifyGatewayAuthorityReceipt(ack, challenge, security.GatewayPendingHash(wire), source) != nil {
		return ErrChannel
	}
	return nil
}
