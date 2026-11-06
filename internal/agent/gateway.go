package agent

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/sagehou/restfleet/internal/domain"
)

var ErrGatewaySession = errors.New("gateway backup session unavailable")
var ErrGatewayCleanup = errors.New("gateway backup session cleanup unconfirmed")
var ErrGatewayBusy = errors.New("gateway backup session admission busy")
var ErrGatewayUnavailable = errors.New("gateway temporarily unavailable")
var ErrGatewayRateLimited = errors.New("gateway temporarily rate limited")

type gatewaySessionResponse struct {
	SessionID    uuid.UUID `json:"session_id"`
	State        string    `json:"state"`
	EndpointPath string    `json:"endpoint_path"`
	ExpiresAt    time.Time `json:"expires_at"`
}

// WithGatewaySession reuses local enrollment identity and accepted repository
// configuration. Only a verified, current-session HTTPS endpoint is lent to
// run. No Control API request, reenrollment or second communication secret is
// needed. The caller must join Restic before returning, including cancellation.
func (s *State) WithGatewaySession(ctx context.Context, operation uuid.UUID, deadline time.Time,
	run func(context.Context, string) error,
) (result error) {
	if !domain.ValidOperationID(operation) || run == nil || !deadline.After(time.Now()) || ctx.Err() != nil {
		return ErrGatewaySession
	}
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	identity, err := LoadIdentity(s)
	if err != nil {
		return ErrGatewaySession
	}
	credential, found, err := s.LoadRepositoryCredential(identity)
	if err != nil || !found {
		return ErrGatewaySession
	}
	defer credential.Clear()
	certificate, _, err := TLSIdentity(s, identity)
	if err != nil {
		return ErrGatewaySession
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(credential.CABundlePEM) {
		return ErrGatewaySession
	}
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	transport := &http.Transport{DialContext: dialer.DialContext,
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots, Certificates: []tls.Certificate{certificate}},
		TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 25 * time.Second,
		MaxResponseHeaderBytes: 16 << 10, MaxConnsPerHost: 2, IdleConnTimeout: time.Minute, DisableCompression: true}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return ErrGatewaySession }}
	basePath := "/restic/" + credential.GatewayID.String() + "/" + credential.RepositoryID.String() + "/"
	origin := strings.TrimSuffix(credential.Endpoint, basePath)
	control := origin + "/gateway/v1/repositories/" + credential.RepositoryID.String() + "/backup-sessions/" + operation.String()
	return withGatewayClientSession(ctx, client, control, origin, basePath, deadline, run)
}

func withGatewayClientSession(ctx context.Context, client *http.Client, control, origin, basePath string, deadline time.Time,
	run func(context.Context, string) error,
) (result error) {
	body, err := json.Marshal(struct {
		Deadline time.Time `json:"deadline"`
	}{deadline.UTC()})
	if err != nil {
		return ErrGatewaySession
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, control, bytes.NewReader(body))
	if err != nil {
		return ErrGatewaySession
	}
	request.Header.Set("Content-Type", "application/json")
	retryUntil := time.Now().Add(30 * time.Second)
	cleanupRequired := false
	executed := false
	// Also clean up an ambiguously accepted POST. A lost response cannot grant
	// permission to start another session with a different operation ID.
	defer func() {
		if !cleanupRequired {
			return
		}
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 25*time.Second)
		defer cancel()
		request, err := http.NewRequestWithContext(cleanup, http.MethodDelete, control, nil)
		if err != nil {
			result = ErrGatewayCleanup
			return
		}
		response, err := client.Do(request)
		if err != nil {
			result = ErrGatewayCleanup
			return
		}
		_ = response.Body.Close()
		if response.StatusCode != http.StatusNoContent {
			result = ErrGatewayCleanup
		} else if executed && errors.Is(result, ErrGatewaySession) {
			if hint := gatewayRetryHint(response); hint != nil {
				result = hint
			}
		}
	}()
	for {
		cleanupRequired = true
		response, err := client.Do(request)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if transientGatewayNetwork(err) {
				var dial *net.OpError
				if request.Method == http.MethodPost && errors.As(err, &dial) && dial.Op == "dial" {
					cleanupRequired = false // No connection, so no request could be accepted.
				}
				return ErrGatewayUnavailable
			}
			return ErrGatewaySession
		}
		raw, readErr := io.ReadAll(io.LimitReader(response.Body, 4097))
		_ = response.Body.Close()
		if readErr == nil && len(raw) <= 4096 {
			if failure := gatewayStatusFailure(response.StatusCode); failure != nil {
				if request.Method == http.MethodPost && (response.StatusCode == http.StatusServiceUnavailable || response.StatusCode == http.StatusTooManyRequests) && gatewayRetryHint(response) == failure {
					cleanupRequired = false
				}
				return failure
			}
		}
		if request.Method == http.MethodPost && response.StatusCode == http.StatusConflict && response.Header.Get("Retry-After") == "1" {
			// Only this response proves no session was consumed. Other conflicts
			// and ambiguous responses retain cleanup and are never retried here.
			cleanupRequired = false
			if readErr != nil || len(raw) > 4096 || time.Until(retryUntil) <= time.Second {
				return ErrGatewayBusy
			}
			select {
			case <-ctx.Done():
				return ErrGatewaySession
			case <-time.After(time.Second):
			}
			request, err = http.NewRequestWithContext(ctx, http.MethodPost, control, bytes.NewReader(body))
			if err != nil {
				return ErrGatewaySession
			}
			request.Header.Set("Content-Type", "application/json")
			continue
		}
		var session gatewaySessionResponse
		if readErr != nil || len(raw) > 4096 || (response.StatusCode != http.StatusOK && response.StatusCode != http.StatusAccepted) ||
			json.Unmarshal(raw, &session) != nil || session.SessionID.Version() != 7 || session.SessionID.Variant() != uuid.RFC4122 ||
			!session.ExpiresAt.After(time.Now()) || session.ExpiresAt.After(deadline) || time.Until(session.ExpiresAt) > 24*time.Hour {
			return ErrGatewaySession
		}
		switch session.State {
		case "ACTIVE":
			if session.EndpointPath != basePath+session.SessionID.String()+"/" {
				return ErrGatewaySession
			}
			work, cancel := context.WithDeadline(ctx, session.ExpiresAt)
			defer cancel()
			executed = true
			if run(work, origin+session.EndpointPath) != nil || work.Err() != nil {
				return ErrGatewaySession
			}
			return nil
		case "STARTING":
			if session.EndpointPath != "" {
				return ErrGatewaySession
			}
		default:
			return ErrGatewaySession
		}
		select {
		case <-ctx.Done():
			return ErrGatewaySession
		case <-time.After(time.Second):
		}
		request, err = http.NewRequestWithContext(ctx, http.MethodGet, control, nil)
		if err != nil {
			return ErrGatewaySession
		}
	}
}

func gatewayStatusFailure(status int) error {
	switch status {
	case http.StatusTooManyRequests:
		return ErrGatewayRateLimited
	case http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return ErrGatewayUnavailable
	}
	return nil
}

func gatewayRetryHint(response *http.Response) error {
	values := response.Header.Values("Restfleet-Backup-Retry")
	if len(values) == 1 {
		switch values[0] {
		case "GATEWAY_UNAVAILABLE":
			return ErrGatewayUnavailable
		case "GATEWAY_RATE_LIMITED":
			return ErrGatewayRateLimited
		}
	}
	return nil
}

func transientGatewayNetwork(err error) bool {
	for _, code := range []syscall.Errno{syscall.ECONNREFUSED, syscall.ECONNRESET, syscall.ENETUNREACH, syscall.EHOSTUNREACH, syscall.ETIMEDOUT, syscall.EPIPE} {
		if errors.Is(err, code) {
			return true
		}
	}
	var network net.Error
	if errors.As(err, &network) && network.Timeout() {
		return true
	}
	var dns *net.DNSError
	return errors.As(err, &dns) && dns.IsTemporary
}

func gatewayFailureCode(err error) string {
	if errors.Is(err, ErrGatewayUnavailable) {
		return "GATEWAY_UNAVAILABLE"
	}
	if errors.Is(err, ErrGatewayRateLimited) {
		return "GATEWAY_RATE_LIMITED"
	}
	return ""
}
