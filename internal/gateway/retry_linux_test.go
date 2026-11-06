package gateway

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestSessionRetryObservationsRejectUnknownAndAmbiguousMutation(t *testing.T) {
	for _, test := range []struct {
		name                     string
		status                   int
		method, path, body, want string
	}{
		{"read throttled", 429, "GET", "config", "", "GATEWAY_RATE_LIMITED"},
		{"read server failure", 500, "GET", "config", "", "GATEWAY_UNAVAILABLE"},
		{"read unavailable", 503, "GET", "config", "", "GATEWAY_UNAVAILABLE"},
		{"read forbidden", 403, "GET", "config", "", ""},
		{"read unauthorized", 401, "GET", "config", "", ""},
		{"read unknown", 418, "GET", "config", "", ""},
		{"missing list cannot hide locks", 404, "GET", "locks/", "", ""},
		{"probe throttled before mutation", 429, "POST", object("data", "payload"), "payload", "GATEWAY_RATE_LIMITED"},
		{"uncertain create", 503, "POST", object("data", "payload"), "payload", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			var posts atomic.Int32
			socket := socketServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodHead && test.name == "uncertain create" {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				if r.Method == http.MethodPost {
					posts.Add(1)
				}
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte("provider-error-canary"))
			}))
			s, secret := newSession(t, socket, bindingFixture(), acceptAudit)
			w := httptest.NewRecorder()
			s.ServeHTTP(w, request(s, secret, test.method, test.path, test.body))
			if strings.Contains(w.Body.String(), "provider-error-canary") || w.Header().Get(backupRetryHeader) != "" {
				t.Fatal("raw error or premature retry hint escaped on data path")
			}
			s.Close()
			if got := s.retry.code(); got != test.want {
				t.Fatalf("retry classification = %q, want %q", got, test.want)
			}
			if test.name == "uncertain create" && posts.Load() != 1 || test.name == "probe throttled before mutation" && posts.Load() != 0 {
				t.Fatal("probe failure unexpectedly reached mutation or mutation was not exercised")
			}
		})
	}
}

func TestSessionPermanentFailureDominatesConcurrentRetryObservations(t *testing.T) {
	s, secret, _ := fixture(t)
	var joined sync.WaitGroup
	for _, status := range []int{429, 500, 503} {
		joined.Go(func() { s.retry.observe(context.Background(), http.MethodGet, &http.Response{StatusCode: status}, nil) })
	}
	joined.Wait()
	if s.retry.code() != "GATEWAY_RATE_LIMITED" {
		t.Fatal("concurrent transient observations lost their priority")
	}
	serve(t, s, request(s, secret, http.MethodDelete, object("locks", "preexisting"), ""), 403)
	if s.retry.code() != "" {
		t.Fatal("lock authorization denial became retryable")
	}
	var unknown backupRetryObservation
	unknown.observe(context.Background(), http.MethodGet, nil, errors.New("timeout 429 503 provider-error-canary"))
	unknown.observe(context.Background(), http.MethodGet, &http.Response{StatusCode: 429}, nil)
	if unknown.code() != "" {
		t.Fatal("unknown transport text became retryable")
	}
}

func TestSessionSnapshotCommitSuppressesEarlierTransientRetryHint(t *testing.T) {
	s, secret, backend := fixture(t)
	s.retry.observe(context.Background(), http.MethodGet, &http.Response{StatusCode: 429}, nil)
	path := object("snapshots", "encrypted snapshot")
	serve(t, s, request(s, secret, http.MethodPost, path, "encrypted snapshot"), 200)
	s.Close()
	if backend.inspect().objects[path] != "encrypted snapshot" || s.retry.code() != "" {
		t.Fatal("committed snapshot gained automatic retry permission")
	}
}
