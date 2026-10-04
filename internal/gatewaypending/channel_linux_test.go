package gatewaypending

import (
	"bytes"
	"context"
	"encoding/binary"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

func replaySocket(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "rfg-ipc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "replay.sock")
}

func serveReplay(t *testing.T, path string, uid uint32, handler func(context.Context, uuid.UUID, []byte) ([]byte, error)) (context.CancelFunc, <-chan error) {
	t.Helper()
	listener, err := ListenReplay(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- ServeReplay(ctx, listener, uid, handler, func(context.Context) error { return nil }) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(6 * time.Second):
			t.Error("replay shutdown hung")
		}
	})
	return cancel, done
}

func TestReplayChannelUIDVersionBoundsAndShutdown(t *testing.T) {
	path := replaySocket(t)
	runtime := uuid.Must(uuid.NewV7())
	uid := uint32(os.Geteuid())
	var calls atomic.Int64
	_, _ = serveReplay(t, path, uid, func(_ context.Context, id uuid.UUID, wire []byte) ([]byte, error) {
		calls.Add(1)
		if id != runtime || string(wire) != "encrypted-record" {
			t.Error("frame changed")
		}
		return []byte("signed-receipt"), nil
	})
	ack, err := Replay(context.Background(), path, uid, runtime, []byte("encrypted-record"))
	if err != nil || string(ack) != "signed-receipt" {
		t.Fatal("local replay failed")
	}
	for _, mutate := range []func([]byte){
		func(h []byte) { h[0] = 'X' },
		func(h []byte) { binary.BigEndian.PutUint32(h[4:8], 2) },
		func(h []byte) { binary.BigEndian.PutUint32(h[24:], 1<<30) },
		func(h []byte) { binary.BigEndian.PutUint32(h[24:], 0) },
		func(h []byte) { clear(h[8:24]) },
	} {
		var raw bytes.Buffer
		if writeFrame(&raw, runtime, []byte("encrypted-record")) != nil {
			t.Fatal("fixture frame")
		}
		h := raw.Bytes()
		mutate(h)
		conn, err := net.Dial("unix", path)
		if err != nil {
			t.Fatal(err)
		}
		_ = conn.SetDeadline(time.Now().Add(time.Second))
		_, _ = conn.Write(h)
		var b [1]byte
		if _, err = conn.Read(b[:]); err == nil {
			t.Fatal("invalid frame accepted")
		}
		_ = conn.Close()
	}
	if calls.Load() != 1 {
		t.Fatal("invalid frame reached central handler")
	}
	if _, err = Replay(context.Background(), path, uid+1, runtime, []byte("encrypted-record")); err != ErrChannel {
		t.Fatal("wrong server UID accepted")
	}
	if _, err = ListenReplay(path); err != ErrChannel {
		t.Fatal("live socket overwritten")
	}
}

func TestReplayChannelRejectsClientUIDAndPrivatePath(t *testing.T) {
	path := replaySocket(t)
	uid := uint32(os.Geteuid())
	runtime := uuid.Must(uuid.NewV7())
	var called atomic.Bool
	_, _ = serveReplay(t, path, uid+1, func(context.Context, uuid.UUID, []byte) ([]byte, error) {
		called.Store(true)
		return []byte("unexpected"), nil
	})
	if _, err := Replay(context.Background(), path, uid, runtime, []byte("encrypted-record")); err != ErrChannel || called.Load() {
		t.Fatal("wrong client UID reached handler")
	}
	if os.Chmod(filepath.Dir(path), 0755) != nil {
		t.Fatal("chmod")
	}
	if _, err := Replay(context.Background(), path, uid, runtime, []byte("encrypted-record")); err != ErrChannel {
		t.Fatal("unsafe directory accepted")
	}
	if os.Chmod(filepath.Dir(path), 0700) != nil {
		t.Fatal("chmod")
	}
	link := filepath.Join(filepath.Dir(path), "link.sock")
	if os.Symlink(path, link) != nil {
		t.Fatal("symlink")
	}
	if _, err := Replay(context.Background(), link, uid, runtime, []byte("encrypted-record")); err != ErrChannel {
		t.Fatal("symlink socket accepted")
	}
}

func TestReplayChannelCancellationJoinsPartialAndBusyConnections(t *testing.T) {
	path := replaySocket(t)
	listener, err := ListenReplay(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- ServeReplay(ctx, listener, uint32(os.Geteuid()), func(context.Context, uuid.UUID, []byte) ([]byte, error) {
			t.Error("partial frame handled")
			return nil, nil
		}, func(context.Context) error { return nil })
	}()
	var clients []net.Conn
	for range 6 {
		conn, err := net.Dial("unix", path)
		if err != nil {
			t.Fatal(err)
		}
		clients = append(clients, conn)
		_, _ = conn.Write([]byte("RF"))
	}
	defer func() {
		for _, conn := range clients {
			_ = conn.Close()
		}
	}()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("partial connections block shutdown")
	}
}

func TestSharedSocketPolicyRejectsPermissionsGroupOwnerAndDowngrade(t *testing.T) {
	group := uint32(os.Getegid())
	if group == 0 {
		t.Skip("shared-group mode requires a non-root group; Actions also runs the cross-UID test")
	}
	path := replaySocket(t)
	dir := filepath.Dir(path)
	if os.Chown(dir, -1, int(group)) != nil || os.Chmod(dir, 0710) != nil {
		t.Fatal("shared directory fixture")
	}
	listener, err := ListenReplay(path, group)
	if err != nil {
		t.Fatal("shared socket refused")
	}
	defer listener.Close()
	uid := uint32(os.Geteuid())
	if !privateReplayPath(path, uid, true, group) || privateReplayPath(path, uid, true) ||
		privateReplayPath(path, uid+1, true, group) || privateReplayPath(path, uid, true, group+1) ||
		privateReplayPath(path, uid, true, group, group) || privateReplayPath(path, uid, true, ^uint32(0)) {
		t.Fatal("shared group or owner validation failed")
	}
	for _, mode := range []os.FileMode{0700, 0750, 0730, 0770, 0711, 0755, 0777, 0710 | os.ModeSetgid, 0710 | os.ModeSticky} {
		if os.Chmod(dir, mode) != nil {
			t.Fatal("directory permissions fixture")
		}
		if privateReplayPath(path, uid, true, group) {
			t.Fatal("unsafe directory accepted")
		}
	}
	if os.Chmod(dir, 0710) != nil {
		t.Fatal("directory reset")
	}
	for _, mode := range []os.FileMode{0600, 0666, 0770, 0660 | os.ModeSetgid} {
		if os.Chmod(path, mode) != nil {
			t.Fatal("socket permissions fixture")
		}
		if privateReplayPath(path, uid, true, group) {
			t.Fatal("unsafe socket accepted")
		}
	}
}
