package rclone

import (
	"context"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// webDAVRelay pins a private byte relay to a validated public destination.
// rclone retains end-to-end TLS, hostname verification and downgrade protection;
// no HTTP request, credential or provider response is interpreted or logged here.
func (r *Runtime) webDAVRelay(ctx context.Context, c *Config, dir string) (string, func(), error) {
	if c.Backend() != "webdav" {
		return "", func() {}, nil
	}
	endpoint, ok := webDAVURL(c.cloud()["url"])
	if !ok {
		return "", nil, ErrUnsafeRuntime
	}
	resolve := r.lookupWebDAV
	if resolve == nil {
		resolve = func(ctx context.Context, host string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		}
	}
	resolveCtx, stopResolve := context.WithTimeout(ctx, 10*time.Second)
	addresses, err := resolve(resolveCtx, endpoint.Hostname())
	stopResolve()
	if err != nil || len(addresses) == 0 || len(addresses) > 32 {
		return "", nil, ErrUnsafeRuntime
	}
	for _, address := range addresses {
		if !publicIP(address) {
			return "", nil, ErrUnsafeRuntime
		}
	}
	port := endpoint.Port()
	if port == "" {
		port = "443"
	}
	// Do not resolve again during dial: this closes the DNS rebinding window.
	target := net.JoinHostPort(addresses[0].Unmap().String(), port)
	dial := r.dialWebDAV
	if dial == nil {
		dial = func(ctx context.Context, address string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "tcp", address)
		}
	}
	socket := filepath.Join(dir, "webdav.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		return "", nil, ErrUnsafeRuntime
	}
	if os.Chmod(socket, 0600) != nil {
		_ = listener.Close()
		return "", nil, ErrUnsafeRuntime
	}
	relayCtx, cancel := context.WithCancel(ctx)
	closeListener := context.AfterFunc(relayCtx, func() { _ = listener.Close() })
	done := make(chan struct{})
	// ponytail: bound this per-command relay; raise the limit only with measured concurrency.
	slots := make(chan struct{}, 16)
	go func() {
		defer close(done)
		var active sync.WaitGroup
		defer active.Wait()
		for {
			local, err := listener.Accept()
			if err != nil {
				return
			}
			select {
			case slots <- struct{}{}:
			default:
				_ = local.Close()
				continue
			}
			active.Add(1)
			go func() {
				defer active.Done()
				defer func() { <-slots; _ = local.Close() }()
				upstream, err := dial(relayCtx, target)
				if err != nil {
					return
				}
				defer func() { _ = upstream.Close() }()
				abort := context.AfterFunc(relayCtx, func() { _ = local.Close(); _ = upstream.Close() })
				defer abort()
				copied := make(chan struct{})
				go func() {
					_, _ = io.Copy(upstream, local)
					_ = upstream.Close()
					_ = local.Close()
					close(copied)
				}()
				_, _ = io.Copy(local, upstream)
				_ = local.Close()
				_ = upstream.Close()
				<-copied
			}()
		}
	}()
	return socket, func() { cancel(); _ = listener.Close(); closeListener(); <-done }, nil
}

func (c *Config) runtimeBytes(socket string) []byte {
	if socket == "" {
		return c.Bytes()
	}
	c.cloud()["unix_socket"] = socket
	defer delete(c.cloud(), "unix_socket")
	return c.Bytes()
}
