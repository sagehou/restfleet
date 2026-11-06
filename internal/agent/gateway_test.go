package agent

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestGatewayRetryRequiresNoAdmissionOrConfirmedCleanup(t *testing.T) {
	for _, test := range []struct {
		name          string
		status        int
		hint          string
		cleanup       int
		cleanupHints  []string
		runFails      bool
		want          error
		wantDeletes   int32
	}{
		{name: "not admitted unavailable", status: 503, hint: "GATEWAY_UNAVAILABLE", want: ErrGatewayUnavailable},
		{name: "not admitted rate limited", status: 429, hint: "GATEWAY_RATE_LIMITED", want: ErrGatewayRateLimited},
		{name: "ambiguous admission cleaned", status: 500, cleanup: 204, want: ErrGatewayUnavailable, wantDeletes: 1},
		{name: "ambiguous admission not cleaned", status: 503, cleanup: 503, want: ErrGatewayCleanup, wantDeletes: 1},
		{name: "wrong status hint cannot skip cleanup", status: 429, hint: "GATEWAY_UNAVAILABLE", cleanup: 503, want: ErrGatewayCleanup, wantDeletes: 1},
		{name: "unsupported no admission status", status: 500, hint: "GATEWAY_UNAVAILABLE", cleanup: 503, want: ErrGatewayCleanup, wantDeletes: 1},
		{name: "permission hint rejected", status: 403, hint: "GATEWAY_RATE_LIMITED", cleanup: 204, want: ErrGatewaySession, wantDeletes: 1},
		{name: "known read failure cleaned", status: 202, cleanup: 204, cleanupHints: []string{"GATEWAY_UNAVAILABLE"}, runFails: true, want: ErrGatewayUnavailable, wantDeletes: 1},
		{name: "provider limit cleaned", status: 202, cleanup: 204, cleanupHints: []string{"GATEWAY_RATE_LIMITED"}, runFails: true, want: ErrGatewayRateLimited, wantDeletes: 1},
		{name: "cleanup failure overrides hint", status: 202, cleanup: 503, cleanupHints: []string{"GATEWAY_RATE_LIMITED"}, runFails: true, want: ErrGatewayCleanup, wantDeletes: 1},
		{name: "successful run keeps result", status: 202, cleanup: 204, cleanupHints: []string{"GATEWAY_RATE_LIMITED"}, wantDeletes: 1},
		{name: "old gateway stays conservative", status: 202, cleanup: 204, runFails: true, want: ErrGatewaySession, wantDeletes: 1},
		{name: "unknown hint ignored", status: 202, cleanup: 204, cleanupHints: []string{"provider-secret-canary"}, runFails: true, want: ErrGatewaySession, wantDeletes: 1},
		{name: "duplicate hint ignored", status: 202, cleanup: 204, cleanupHints: []string{"GATEWAY_UNAVAILABLE", "GATEWAY_UNAVAILABLE"}, runFails: true, want: ErrGatewaySession, wantDeletes: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			deadline := time.Now().UTC().Add(10 * time.Second)
			base := "/restic/" + uuid.Must(uuid.NewV7()).String() + "/" + uuid.Must(uuid.NewV7()).String() + "/"
			control := "/gateway/v1/repositories/" + uuid.Must(uuid.NewV7()).String() + "/backup-sessions/" + uuid.Must(uuid.NewV7()).String()
			session := uuid.Must(uuid.NewV7())
			var posts, deletes atomic.Int32
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != control {
					t.Error("request changed operation binding")
				}
				if r.Method == http.MethodDelete {
					deletes.Add(1)
					for _, hint := range test.cleanupHints {
						w.Header().Add("Restfleet-Backup-Retry", hint)
					}
					w.WriteHeader(test.cleanup)
					return
				}
				posts.Add(1)
				if r.Method != http.MethodPost {
					t.Error("unexpected request method")
				}
				var body struct{ Deadline time.Time `json:"deadline"` }
				if json.NewDecoder(r.Body).Decode(&body) != nil || !body.Deadline.Equal(deadline) {
					t.Error("retry changed original deadline")
				}
				if test.hint != "" {
					w.Header().Set("Restfleet-Backup-Retry", test.hint)
				}
				w.WriteHeader(test.status)
				if test.status == 202 {
					_ = json.NewEncoder(w).Encode(gatewaySessionResponse{SessionID: session, State: "ACTIVE", EndpointPath: base + session.String() + "/", ExpiresAt: deadline.Add(-time.Second)})
				} else {
					_, _ = io.WriteString(w, "provider-secret-canary")
				}
			}))
			defer server.Close()
			ctx, cancel := context.WithDeadline(context.Background(), deadline)
			defer cancel()
			runs := 0
			err := withGatewayClientSession(ctx, server.Client(), server.URL+control, server.URL, base, deadline, func(_ context.Context, endpoint string) error {
				runs++
				if endpoint != server.URL+base+session.String()+"/" {
					t.Error("unverified endpoint lent to executor")
				}
				if test.runFails {
					return errors.New("provider-secret-canary")
				}
				return nil
			})
			if !errors.Is(err, test.want) || deletes.Load() != test.wantDeletes || posts.Load() != 1 || (runs == 1) != (test.status == 202) {
				t.Fatalf("result=%v posts=%d deletes=%d runs=%d", err, posts.Load(), deletes.Load(), runs)
			}
			if err != nil && strings.Contains(err.Error(), "provider-secret-canary") {
				t.Fatal("raw backend failure escaped")
			}
		})
	}
}

type gatewayFailureTransport struct {
	err     error
	cleanup int
	deletes int
}

func (f *gatewayFailureTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method != http.MethodDelete {
		return nil, f.err
	}
	f.deletes++
	return &http.Response{StatusCode: f.cleanup, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(""))}, nil
}

func TestGatewayNetworkRetryDoesNotTreatTLSOrUnknownTextAsTransient(t *testing.T) {
	for _, test := range []struct {
		name string
		failure error
		cleanup, deletes int
		want error
	}{
		{"dial before acceptance", &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}, 0, 0, ErrGatewayUnavailable},
		{"reset after acceptance", &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}, 204, 1, ErrGatewayUnavailable},
		{"reset without cleanup", &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}, 503, 1, ErrGatewayCleanup},
		{"certificate failure", x509.UnknownAuthorityError{}, 204, 1, ErrGatewaySession},
		{"unknown error text", errors.New("network timeout 429 503 provider-secret-canary"), 204, 1, ErrGatewaySession},
	} {
		t.Run(test.name, func(t *testing.T) {
			transport := &gatewayFailureTransport{err: test.failure, cleanup: test.cleanup}
			deadline := time.Now().Add(time.Minute)
			err := withGatewayClientSession(context.Background(), &http.Client{Transport: transport}, "https://gateway.invalid/control", "https://gateway.invalid", "/restic/", deadline, func(context.Context, string) error {
				t.Fatal("executor ran after admission failed")
				return nil
			})
			if !errors.Is(err, test.want) || transport.deletes != test.deletes {
				t.Fatalf("result=%v deletes=%d", err, transport.deletes)
			}
		})
	}
}
