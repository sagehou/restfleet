package gatewaypending

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/sagehou/restfleet/internal/security"
)

var ErrChannel = errors.New("gateway replay channel unavailable")

// ListenReplay binds only a NEW socket in a canonical service-owned 0700
// directory. It never removes an existing socket or takes over another owner.
func ListenReplay(path string) (*net.UnixListener, error) {
	if !privateReplayPath(path, uint32(os.Geteuid()), false) {
		return nil, ErrChannel
	}
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, ErrChannel
	}
	if os.Chmod(path, 0600) != nil {
		_ = l.Close()
		return nil, ErrChannel
	}
	return l, nil
}

func privateReplayPath(path string, owner uint32, existing bool) bool {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return false
	}
	dir := filepath.Dir(path)
	canonical, err := filepath.EvalSymlinks(dir)
	if err != nil || canonical != dir {
		return false
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		return false
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st.Uid != owner {
		return false
	}
	if !existing {
		return true
	}
	info, err = os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0600 {
		return false
	}
	st, ok = info.Sys().(*syscall.Stat_t)
	return ok && st.Uid == owner
}

func peerUID(conn *net.UnixConn, expected uint32) bool {
	raw, err := conn.SyscallConn()
	if err != nil {
		return false
	}
	var credentials *syscall.Ucred
	var credentialErr error
	if raw.Control(func(fd uintptr) {
		credentials, credentialErr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}) != nil {
		return false
	}
	return credentialErr == nil && credentials != nil && credentials.Uid == expected
}

// ServeReplay is a replay-only channel; it carries no plaintext material or
// unsigned ACK. The handler MUST verify the registered source and commit before
// signing its receipt. Four connections, five seconds each; no request logs.
func ServeReplay(ctx context.Context, listener *net.UnixListener, expectedUID uint32, handler func(context.Context, uuid.UUID, []byte) ([]byte, error), denied func(context.Context) error) error {
	if listener == nil || handler == nil || denied == nil {
		return ErrChannel
	}
	work, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(work, func() { _ = listener.Close() })
	defer stop()
	var wg sync.WaitGroup
	defer wg.Wait()
	defer cancel()
	slots := make(chan struct{}, 4)
	for {
		conn, err := listener.AcceptUnix()
		if err != nil {
			if work.Err() != nil {
				return nil
			}
			return ErrChannel
		}
		select {
		case slots <- struct{}{}:
		default:
			_ = conn.Close()
			continue
		}
		wg.Go(func() {
			defer func() { <-slots }()
			defer conn.Close()
			request, cancel := context.WithTimeout(work, 5*time.Second)
			defer cancel()
			stop := context.AfterFunc(request, func() { _ = conn.Close() })
			defer stop()
			_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
			if !peerUID(conn, expectedUID) {
				_ = denied(request)
				return
			}
			runtime, wire, err := readFrame(conn, security.MaxGatewayPendingSize)
			if err != nil || len(wire) == 0 {
				if request.Err() == nil {
					_ = denied(request)
				}
				return
			}
			ack, err := handler(request, runtime, wire)
			if err != nil || request.Err() != nil || len(ack) == 0 || len(ack) > 1024 {
				return
			}
			_ = writeFrame(conn, runtime, ack)
		})
	}
}

func writeFrame(w io.Writer, runtime uuid.UUID, body []byte) error {
	return writeChannelFrame(w, "RFGR", runtime, body)
}

func writeChannelFrame(w io.Writer, magic string, runtime uuid.UUID, body []byte) error {
	var header [28]byte
	copy(header[:4], magic)
	binary.BigEndian.PutUint32(header[4:8], 1)
	copy(header[8:24], runtime[:])
	binary.BigEndian.PutUint32(header[24:], uint32(len(body)))
	_, err := io.Copy(w, io.MultiReader(bytes.NewReader(header[:]), bytes.NewReader(body)))
	return err
}

func readFrame(r io.Reader, max uint32) (uuid.UUID, []byte, error) {
	return readChannelFrame(r, "RFGR", max)
}

func readChannelFrame(r io.Reader, magic string, max uint32) (uuid.UUID, []byte, error) {
	var header [28]byte
	var runtime uuid.UUID
	if _, err := io.ReadFull(r, header[:]); err != nil || string(header[:4]) != magic || binary.BigEndian.Uint32(header[4:8]) != 1 {
		return runtime, nil, ErrChannel
	}
	copy(runtime[:], header[8:24])
	size := binary.BigEndian.Uint32(header[24:])
	if runtime.Version() != 7 || runtime.Variant() != uuid.RFC4122 || size == 0 || size > max {
		return uuid.Nil, nil, ErrChannel
	}
	body := make([]byte, size)
	if _, err := io.ReadFull(r, body); err != nil {
		return uuid.Nil, nil, ErrChannel
	}
	return runtime, body, nil
}

func Replay(ctx context.Context, path string, serverUID uint32, runtime uuid.UUID, wire []byte) ([]byte, error) {
	if len(wire) == 0 || len(wire) > security.MaxGatewayPendingSize || !privateReplayPath(path, serverUID, true) {
		return nil, ErrChannel
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	dialer := net.Dialer{}
	connection, err := dialer.DialContext(ctx, "unix", path)
	if err != nil {
		return nil, ErrChannel
	}
	defer connection.Close()
	conn, ok := connection.(*net.UnixConn)
	if !ok || !peerUID(conn, serverUID) {
		return nil, ErrChannel
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	deadline, _ := ctx.Deadline()
	_ = conn.SetDeadline(deadline)
	if writeFrame(conn, runtime, wire) != nil {
		return nil, ErrChannel
	}
	returned, ack, err := readFrame(conn, 1024)
	if err != nil || returned != runtime || ctx.Err() != nil {
		return nil, ErrChannel
	}
	return ack, nil
}

func Drain(ctx context.Context, queue *Queue, path string, serverUID uint32, runtime uuid.UUID) error {
	if queue == nil {
		return ErrChannel
	}
	for {
		if ctx.Err() != nil {
			return ErrChannel
		}
		wire, err := queue.Next()
		if err != nil {
			return ErrQueue
		}
		if wire == nil {
			return nil
		}
		ack, err := Replay(ctx, path, serverUID, runtime, wire)
		if err != nil {
			return ErrChannel
		}
		if queue.Acknowledge(ack) != nil {
			return ErrQueue
		}
	}
}
