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
	"time"

	"github.com/google/uuid"
)

var ErrGatewaySession = errors.New("gateway backup session unavailable")

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
	if operation.Version() != 7 || operation.Variant() != uuid.RFC4122 || run == nil || !deadline.After(time.Now()) || ctx.Err() != nil {
		return ErrGatewaySession
	}
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
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots, Certificates: []tls.Certificate{certificate}},
		TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 25 * time.Second,
		MaxResponseHeaderBytes: 16 << 10, MaxConnsPerHost: 2, IdleConnTimeout: time.Minute, DisableCompression: true}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return ErrGatewaySession }}
	basePath := "/restic/" + credential.GatewayID.String() + "/" + credential.RepositoryID.String() + "/"
	origin := strings.TrimSuffix(credential.Endpoint, basePath)
	control := origin + "/gateway/v1/repositories/" + credential.RepositoryID.String() + "/backup-sessions/" + operation.String()
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
	// Also clean up an ambiguously accepted POST. A lost response cannot grant
	// permission to start another session with a different operation ID.
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 25*time.Second)
		defer cancel()
		request, err := http.NewRequestWithContext(cleanup, http.MethodDelete, control, nil)
		if err != nil {
			result = ErrGatewaySession
			return
		}
		response, err := client.Do(request)
		if err != nil {
			result = ErrGatewaySession
			return
		}
		_ = response.Body.Close()
		if response.StatusCode != http.StatusNoContent {
			result = ErrGatewaySession
		}
	}()
	for {
		response, err := client.Do(request)
		if err != nil {
			return ErrGatewaySession
		}
		raw, readErr := io.ReadAll(io.LimitReader(response.Body, 4097))
		_ = response.Body.Close()
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
