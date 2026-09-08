package gateway

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

type notifyingBody struct {
	io.ReadCloser
	once    sync.Once
	started chan struct{}
}

func (b *notifyingBody) Read(p []byte) (int, error) {
	b.once.Do(func() { close(b.started) })
	return b.ReadCloser.Read(p)
}

func TestSessionCloseInterruptsStalledIncomingUpload(t *testing.T) {
	s, secret, b := fixture(t)
	started := make(chan struct{})
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = &notifyingBody{ReadCloser: r.Body, started: started}
		s.ServeHTTP(w, r)
	}))
	defer server.Close()
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+s.prefix+object("data", "pending"), reader)
	if err != nil {
		t.Fatal("invalid fixture request")
	}
	r.ContentLength = 7
	r.SetBasicAuth(s.binding.GatewayID.String(), string(secret))
	done := make(chan struct{})
	go func() {
		resp, err := server.Client().Do(r)
		if err == nil {
			_ = resp.Body.Close()
		}
		close(done)
	}()
	go func() { _, _ = writer.Write([]byte("p")) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("upload did not start")
	}
	closed := make(chan struct{})
	go func() { s.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("close did not interrupt incoming body")
	}
	_ = writer.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("upload client did not stop")
	}
	if len(b.inspect().calls) != 0 {
		t.Fatal("unvalidated upload reached backend")
	}
}

func TestSessionRequestBudgetAndCancellation(t *testing.T) {
	started := make(chan struct{}, 8)
	socket := socketServer(t, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		<-r.Context().Done()
	}))
	s, secret := newSession(t, socket, bindingFixture(), acceptAudit)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() { s.ServeHTTP(httptest.NewRecorder(), request(s, secret, http.MethodGet, "config", "")) })
	}
	for range 8 {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("request did not start")
		}
	}
	serve(t, s, request(s, secret, http.MethodGet, "config", ""), http.StatusTooManyRequests)
	s.Close()
	wg.Wait()
}

func TestSessionDeadlineRemovesAuthorization(t *testing.T) {
	socket := socketServer(t, &fakeBackend{objects: map[string]string{}})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	s, secret, err := NewBackupSession(ctx, bindingFixture(), socket, acceptAudit)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	<-ctx.Done()
	serve(t, s, request(s, secret, http.MethodDelete, object("locks", "expired"), ""), http.StatusForbidden)
}

func TestSessionDoesNotTranslateListFailureIntoEmptyRepository(t *testing.T) {
	socket := socketServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "provider-error-canary", http.StatusNotFound)
	}))
	s, secret := newSession(t, socket, bindingFixture(), acceptAudit)
	serve(t, s, request(s, secret, http.MethodGet, "locks/", ""), http.StatusBadGateway)
	serve(t, s, request(s, secret, http.MethodGet, "snapshots/", ""), http.StatusBadGateway)
}
