package gateway

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func publicKeyPair(t *testing.T, before, after time.Time) ([]byte, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), IsCA: true, BasicConstraintsValid: true,
		NotBefore: before, NotAfter: after, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	private, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: private})
}

type publicFixture struct {
	URL         string
	certificate []byte
	client      *http.Client
	server      *PublicServer
	listener    net.Listener
	cancel      context.CancelFunc
	done        chan struct{}
	err         error // Read only after done closes.
}

func startPublicFixture(t *testing.T, s *Supervisor, configure func(*PublicServer)) *publicFixture {
	t.Helper()
	cert, key := publicKeyPair(t, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	server, err := NewPublicServer(s, cert, key)
	clear(key)
	if err != nil {
		t.Fatal(err)
	}
	if configure != nil {
		configure(server)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(cert) {
		t.Fatal("invalid fixture CA")
	}
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}, ForceAttemptHTTP2: true}
	ctx, cancel := context.WithCancel(context.Background())
	f := &publicFixture{URL: "https://" + listener.Addr().String(), certificate: cert, server: server, listener: listener,
		client: &http.Client{Transport: transport, Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		cancel: cancel, done: make(chan struct{})}
	go func() { f.err = server.Serve(ctx, listener); close(f.done) }()
	t.Cleanup(func() { transport.CloseIdleConnections(); f.Close(t) })
	return f
}

func (f *publicFixture) Close(t *testing.T) {
	t.Helper()
	f.cancel()
	select {
	case <-f.done:
	case <-time.After(10 * time.Second):
		t.Fatal("public server shutdown hung")
	}
}

func (f *publicFixture) tlsConfig() *tls.Config {
	return f.client.Transport.(*http.Transport).TLSClientConfig.Clone()
}

func TestPublicCertificateValidation(t *testing.T) {
	s := &Supervisor{}
	cert, key := publicKeyPair(t, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	_, differentKey := publicKeyPair(t, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	expired, expiredKey := publicKeyPair(t, time.Now().Add(-2*time.Hour), time.Now().Add(-time.Hour))
	future, futureKey := publicKeyPair(t, time.Now().Add(time.Hour), time.Now().Add(2*time.Hour))
	for _, fixture := range []struct{ cert, key []byte }{{nil, nil}, {cert, []byte("secret-canary")}, {cert, differentKey}, {expired, expiredKey}, {future, futureKey}} {
		if _, err := NewPublicServer(s, fixture.cert, fixture.key); err != ErrPublicTLS {
			t.Fatalf("unsafe key pair: %v", err)
		}
	}
	if _, err := NewPublicServer(nil, cert, key); err != ErrInvalidSession {
		t.Fatal("nil supervisor accepted")
	}
	p, err := NewPublicServer(s, cert, key)
	if err != nil {
		t.Fatal(err)
	}
	if p.connections != 128 || p.rate != 256 || p.http.ReadHeaderTimeout != 5*time.Second || p.http.IdleTimeout != time.Minute ||
		p.http.ReadTimeout != 30*time.Second || p.http.WriteTimeout != 30*time.Second || p.http.MaxHeaderBytes != 16<<10 {
		t.Fatal("public resource defaults changed")
	}
}

func TestPublicTLSAndRawRouting(t *testing.T) {
	s, _, _ := supervisorFixture(t, "success", 1)
	f := startPublicFixture(t, s, nil)
	op := startSupervised(t, s, backupFixture(), noGatewayRefresh)
	access := waitAccess(t, op)
	for _, check := range []struct {
		path       string
		authorized bool
		status     int
	}{
		{access.EndpointPath + "config", true, 200}, {access.EndpointPath + "config", false, 401},
		{access.EndpointPath + "../config", true, 403}, {access.EndpointPath + "%63onfig", true, 403},
		{access.EndpointPath + "config?secret-canary", true, 403}, {"/metrics", true, 403}, {"/healthz", true, 403}, {"/api/v1/repositories", true, 403},
	} {
		req, err := http.NewRequest(http.MethodGet, f.URL+check.path, nil)
		if err != nil {
			t.Fatal(err)
		}
		if check.authorized {
			req.SetBasicAuth(access.Username, string(access.Password))
		}
		req.Header.Set("X-Forwarded-For", "127.0.0.1")
		req.Header.Set("X-Forwarded-Proto", "https")
		resp, err := f.client.Do(req)
		if err != nil {
			t.Fatal("TLS request failed")
		}
		body, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if readErr != nil || resp.StatusCode != check.status || resp.ProtoMajor != 1 || resp.TLS == nil || resp.TLS.Version < tls.VersionTLS12 ||
			resp.Header.Get("Location") != "" || resp.Header.Get("Cache-Control") != "no-store" || strings.Contains(string(body), "secret-canary") {
			t.Fatalf("unsafe public response, status=%d", resp.StatusCode)
		}
	}
	for _, kind := range []string{"old TLS", "wrong CA", "HTTP2 only"} {
		config := f.tlsConfig()
		switch kind {
		case "old TLS":
			config.MinVersion, config.MaxVersion = tls.VersionTLS10, tls.VersionTLS11
		case "wrong CA":
			config.RootCAs = x509.NewCertPool()
		case "HTTP2 only":
			config.NextProtos = []string{"h2"}
		}
		conn, err := tls.DialWithDialer(&net.Dialer{Timeout: time.Second}, "tcp", f.listener.Addr().String(), config)
		if err == nil {
			_ = conn.Close()
			t.Fatalf("accepted %s", kind)
		}
	}
	plain := &http.Client{Timeout: time.Second}
	resp, err := plain.Get(strings.Replace(f.URL, "https:", "http:", 1) + access.EndpointPath + "config")
	if err == nil {
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatal("plaintext request accepted")
		}
	}
	req, err := http.NewRequest(http.MethodGet, f.URL+access.EndpointPath+"config", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Large", strings.Repeat("x", 64<<10))
	resp, err = f.client.Do(req)
	if err != nil {
		t.Fatal("large header probe failed")
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusRequestHeaderFieldsTooLarge {
		t.Fatal("oversized headers reached handler")
	}
	finishSupervised(t, op)
}

func TestPublicConnectionCapIncludesHandshakes(t *testing.T) {
	s, _, _ := supervisorFixture(t, "success", 1)
	accepted := make(chan struct{}, 10)
	f := startPublicFixture(t, s, func(p *PublicServer) {
		p.connections = 2
		p.http.ConnState = func(_ net.Conn, state http.ConnState) {
			if state == http.StateNew {
				accepted <- struct{}{}
			}
		}
	})
	open := func() net.Conn {
		conn, err := net.DialTimeout("tcp", f.listener.Addr().String(), time.Second)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		select {
		case <-accepted:
		case <-time.After(time.Second):
			t.Fatal("connection not accepted")
		}
		return conn
	}
	first := open()
	_ = open()
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 150 * time.Millisecond}, "tcp", f.listener.Addr().String(), f.tlsConfig())
	if err == nil {
		_ = conn.Close()
		t.Fatal("connection cap bypassed during handshake")
	}
	_ = first.Close()
	conn, err = tls.DialWithDialer(&net.Dialer{Timeout: time.Second}, "tcp", f.listener.Addr().String(), f.tlsConfig())
	if err != nil {
		t.Fatal("closed connection did not release capacity")
	}
	_ = conn.Close()
	f.Close(t) // Must unblock a saturated LimitListener.Accept as well.
	if !errors.Is(f.err, context.Canceled) {
		t.Fatalf("shutdown: %v", f.err)
	}
}

func TestPublicHandshakeHeaderAndIdleTimeouts(t *testing.T) {
	for _, kind := range []string{"handshake", "header", "idle"} {
		t.Run(kind, func(t *testing.T) {
			s, _, _ := supervisorFixture(t, "success", 1)
			f := startPublicFixture(t, s, func(p *PublicServer) {
				p.http.ReadHeaderTimeout = 200 * time.Millisecond
				p.http.IdleTimeout = 200 * time.Millisecond
			})
			var conn net.Conn
			var err error
			if kind == "handshake" {
				conn, err = net.DialTimeout("tcp", f.listener.Addr().String(), time.Second)
			} else {
				conn, err = tls.DialWithDialer(&net.Dialer{Timeout: time.Second}, "tcp", f.listener.Addr().String(), f.tlsConfig())
			}
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = conn.Close() }()
			if err := conn.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
				t.Fatal(err)
			}
			reader := bufio.NewReader(conn)
			if kind == "header" {
				_, err = io.WriteString(conn, "GET / HTTP/1.1\r\nHost:")
			}
			if kind == "idle" {
				_, err = io.WriteString(conn, "GET / HTTP/1.1\r\nHost: fixture\r\n\r\n")
				if err != nil {
					t.Fatal(err)
				}
				resp, readErr := http.ReadResponse(reader, nil)
				if readErr != nil {
					t.Fatal(readErr)
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
			}
			if err != nil {
				t.Fatal(err)
			}
			_, err = reader.ReadByte()
			var timeout net.Error
			if err == nil || (errors.As(err, &timeout) && timeout.Timeout()) {
				t.Fatal("server did not close stalled/idle connection")
			}
		})
	}
}

func TestPublicRateLimitBoundedAndAudited(t *testing.T) {
	var admitted, limited atomic.Int32
	s := &Supervisor{audit: func(_ context.Context, e Event) error {
		if e.Binding != (Binding{}) || e.Authenticated || e.Action != "denied" {
			return errors.New("unsafe audit")
		}
		switch e.Reason {
		case "route_unavailable":
			admitted.Add(1)
		case "rate_limited":
			limited.Add(1)
		default:
			return errors.New("unsafe reason")
		}
		return nil
	}}
	cert, key := publicKeyPair(t, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	p, err := NewPublicServer(s, cert, key)
	if err != nil {
		t.Fatal(err)
	}
	p.rate = 4
	// Freeze this window without sleeps; the next assertion explicitly expires it.
	p.window = time.Now().Add(time.Hour)
	var wg sync.WaitGroup
	var forbidden, throttled atomic.Int32
	for i := range 40 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := httptest.NewRequest(http.MethodGet, "/secret-canary", nil)
			r.Header.Set("X-Forwarded-For", fmt.Sprintf("192.0.2.%d", i))
			w := httptest.NewRecorder()
			p.serveHTTP(w, r)
			switch w.Code {
			case 403:
				forbidden.Add(1)
			case 429:
				throttled.Add(1)
			}
		}()
	}
	wg.Wait()
	if forbidden.Load() != 4 || throttled.Load() != 36 || admitted.Load() != 4 || limited.Load() != 1 {
		t.Fatal("unbounded rate/audit work or proxy-header bypass")
	}
	p.window = time.Now().Add(-2 * time.Second)
	w := httptest.NewRecorder()
	p.serveHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if w.Code != 403 || admitted.Load() != 5 {
		t.Fatal("rate window did not recover")
	}
	s.audit = func(context.Context, Event) error { return errors.New("secret-canary") }
	p.mu.Lock()
	p.requests = p.rate
	p.reported = false
	p.mu.Unlock()
	w = httptest.NewRecorder()
	p.serveHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if w.Code != 503 || strings.Contains(w.Body.String(), "secret-canary") {
		t.Fatal("audit failure leaked or permitted request")
	}
}

func TestPublicShutdownJoinsTransferAndRuntime(t *testing.T) {
	for _, failedListener := range []bool{false, true} {
		t.Run(fmt.Sprint(failedListener), func(t *testing.T) {
			s, dir, root := supervisorFixture(t, "success", 1)
			f := startPublicFixture(t, s, nil)
			op := startSupervised(t, s, backupFixture(), noGatewayRefresh)
			access := waitAccess(t, op)
			conn, err := tls.DialWithDialer(&net.Dialer{Timeout: time.Second}, "tcp", f.listener.Addr().String(), f.tlsConfig())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = conn.Close() }()
			req, err := http.NewRequest(http.MethodPost, f.URL+access.EndpointPath+"data/"+strings.Repeat("a", 64), nil)
			if err != nil {
				t.Fatal(err)
			}
			req.SetBasicAuth(access.Username, string(access.Password))
			_, err = fmt.Fprintf(conn, "POST %s HTTP/1.1\r\nHost: fixture\r\nAuthorization: %s\r\nContent-Length: 100\r\n\r\nx", req.URL.Path, req.Header.Get("Authorization"))
			if err != nil {
				t.Fatal("upload fixture failed")
			}
			deadline := time.Now().Add(2 * time.Second)
			for {
				s.mu.Lock()
				session := s.active[backupFixture().Binding.RepositoryID].session
				s.mu.Unlock()
				if len(session.requests) == 1 {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("stalled upload did not enter guard")
				}
				time.Sleep(time.Millisecond)
			}
			if failedListener {
				_ = f.listener.Close()
				select {
				case <-f.done:
				case <-time.After(10 * time.Second):
					t.Fatal("listener failure did not cancel sessions")
				}
				if f.err != ErrPublicServe {
					t.Fatalf("listener failure: %v", f.err)
				}
			} else {
				f.Close(t)
				if !errors.Is(f.err, context.Canceled) {
					t.Fatalf("cancel: %v", f.err)
				}
			}
			waitSupervised(t, op)
			assertSupervisorClean(t, dir, root, backupFixture())
			if !errors.Is(op.err, context.Canceled) {
				t.Fatalf("backup not canceled: %v", op.err)
			}
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			if err := f.server.Serve(context.Background(), listener); err != ErrPublicServe {
				t.Fatal("server reused after revocation")
			}
			if _, err := listener.Accept(); err == nil {
				t.Fatal("rejected listener leaked")
			}
		})
	}
}

func TestPublicRateLimitOverTLS(t *testing.T) {
	var audits atomic.Int32
	s := &Supervisor{audit: func(context.Context, Event) error { audits.Add(1); return nil }}
	f := startPublicFixture(t, s, func(p *PublicServer) { p.rate = 1; p.window = time.Now().Add(time.Hour) })
	for _, want := range []int{403, 429, 429} {
		resp, err := f.client.Get(f.URL + "/")
		if err != nil {
			t.Fatal("rate-limit TLS request failed")
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != want || (want == 429 && (!resp.Close || resp.Header.Get("Retry-After") != "1")) {
			t.Fatal("rate-limit transport response mismatch")
		}
	}
	if audits.Load() != 2 {
		t.Fatal("transport caused an audit amplification")
	}
}

func TestPublicShutdownWaitsForUnauthenticatedAudit(t *testing.T) {
	entered, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	s := &Supervisor{audit: func(ctx context.Context, _ Event) error {
		close(entered)
		<-ctx.Done()
		close(canceled)
		<-release
		return ctx.Err()
	}}
	f := startPublicFixture(t, s, nil)
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	requestDone := make(chan struct{})
	go func() {
		defer close(requestDone)
		resp, err := f.client.Get(f.URL + "/")
		if err == nil {
			_ = resp.Body.Close()
		}
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("audit did not start")
	}
	f.cancel()
	select {
	case <-canceled:
	case <-time.After(2 * time.Second):
		t.Fatal("audit context not canceled")
	}
	select {
	case <-f.done:
		t.Fatal("Serve returned before audit cleanup")
	default:
	}
	once.Do(func() { close(release) })
	f.Close(t)
	select {
	case <-requestDone:
	case <-time.After(2 * time.Second):
		t.Fatal("request did not stop")
	}
}
