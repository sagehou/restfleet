package gateway

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"sync"
	"time"

	"golang.org/x/net/netutil"
)

var (
	ErrPublicTLS   = errors.New("gateway TLS certificate unavailable")
	ErrPublicServe = errors.New("gateway listener failed or already used")
)

// PublicServer owns a single TLS listener and its Supervisor, not the credential
// runtime. The caller MUST establish durable backup admission/auditing before
// exposing this transport; listening does not mean a Repository is READY.
type PublicServer struct {
	http        *http.Server
	supervisor  *Supervisor
	connections int
	rate        int

	mu       sync.Mutex
	started  bool
	stopping bool
	window   time.Time
	requests int
	reported bool
	running  sync.WaitGroup
}

// NewPublicServer validates the borrowed PEM key pair before the caller binds a
// public socket. The parsed key is retained for the server lifetime. Load it
// from protected files; never log PEM, paths, handshake errors or raw requests.
func NewPublicServer(s *Supervisor, certificatePEM, privateKeyPEM []byte) (*PublicServer, error) {
	if s == nil {
		return nil, ErrInvalidSession
	}
	certificate, err := tls.X509KeyPair(certificatePEM, privateKeyPEM)
	if err != nil || certificate.Leaf == nil || time.Now().Before(certificate.Leaf.NotBefore) || !time.Now().Before(certificate.Leaf.NotAfter) {
		return nil, ErrPublicTLS
	}
	p := &PublicServer{supervisor: s, connections: 128, rate: 256}
	// ponytail: HTTP/1.1 bounds in-flight work by the TCP connection cap. Enable
	// HTTP/2 only with explicit stream limits and pinned Restic conformance.
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	p.http = &http.Server{
		Handler:           http.HandlerFunc(p.serveHTTP),
		TLSConfig:         &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{certificate}},
		Protocols:         protocols,
		ReadHeaderTimeout: 5 * time.Second,
		// Authenticated object transfers extend these via ResponseController.
		ReadTimeout:    30 * time.Second,
		WriteTimeout:   30 * time.Second,
		IdleTimeout:    time.Minute,
		MaxHeaderBytes: 16 << 10,
		ErrorLog:       log.New(io.Discard, "", 0),
	}
	return p, nil
}

// Serve takes ownership of listener, serves TLS only, and is single-use. On
// cancellation or listener failure it closes ingress, cancels/joins requests
// and all supervised backends. Only AFTER it returns may the caller close the
// shared credential runtime. Callback implementations MUST honor cancellation.
func (p *PublicServer) Serve(ctx context.Context, listener net.Listener) error {
	if listener == nil {
		return ErrPublicServe
	}
	defer func() { _ = listener.Close() }()
	p.mu.Lock()
	if p.started {
		p.mu.Unlock()
		return ErrPublicServe
	}
	p.started = true
	p.mu.Unlock()

	requestCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	p.http.BaseContext = func(net.Listener) context.Context { return requestCtx }
	stop := context.AfterFunc(ctx, func() { _ = p.http.Close() })
	err := p.http.ServeTLS(netutil.LimitListener(listener, p.connections), "", "")
	stop()
	// Close (not a graceful hour-long Shutdown) revokes active transfers. Gate
	// Add before Wait, including unauthenticated requests and their audit calls.
	p.mu.Lock()
	p.stopping = true
	p.mu.Unlock()
	cancel()
	_ = p.http.Close()
	p.supervisor.Close()
	p.running.Wait()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		return ErrPublicServe // Never leak listener addresses or raw errors.
	}
	return nil
}

func (p *PublicServer) serveHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	p.mu.Lock()
	if p.stopping {
		p.mu.Unlock()
		http.Error(w, "gateway unavailable", http.StatusServiceUnavailable)
		return
	}
	p.running.Add(1)
	now := time.Now()
	if p.window.IsZero() || now.Sub(p.window) >= time.Second {
		p.window, p.requests, p.reported = now, 0, false
	}
	limited := p.requests >= p.rate
	report := limited && !p.reported
	if limited {
		p.reported = true
	} else {
		p.requests++
	}
	p.mu.Unlock()
	defer p.running.Done()
	defer func() {
		// Bound net/http's unread-body drain even when the guard rejected an
		// authenticated mutation after extending its transfer deadline.
		_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(5 * time.Second))
	}()
	if limited {
		// ponytail: one global fixed window, no attacker-controlled key map.
		// Aggregate transport rejections at most once/window without raw inputs;
		// requests admitted to authentication retain per-request security audits.
		w.Header().Set("Connection", "close")
		w.Header().Set("Retry-After", "1")
		if report && !p.supervisor.record(r.Context(), Binding{}, "denied", "rate_limited") {
			http.Error(w, "gateway unavailable", http.StatusServiceUnavailable)
			return
		}
		http.Error(w, "gateway request limited", http.StatusTooManyRequests)
		return
	}
	// Direct dispatch preserves malformed paths for the existing security guard;
	// do not add ServeMux cleaning, redirects, proxy-header trust or admin routes.
	p.supervisor.ServeHTTP(w, r)
}
