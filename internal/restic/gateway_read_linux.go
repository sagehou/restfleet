package restic

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/sagehou/restfleet/internal/domain"
	"github.com/sagehou/restfleet/internal/security"
)

// GatewayReadAccess is borrowed central-only material. A read adapter receiving
// it must never open a second rclone or use the direct-cloud fallback.
type GatewayReadAccess struct {
	Origin                                     string
	Binding                                    domain.GatewayReadBinding
	CertificatePEM, PrivateKeyPEM, CABundlePEM []byte
}

type gatewayReadSession struct {
	SessionID    uuid.UUID `json:"session_id"`
	State        string    `json:"state"`
	EndpointPath string    `json:"endpoint_path"`
	ExpiresAt    time.Time `json:"expires_at"`
}

func (p *Provisioner) withGatewayRepository(ctx context.Context, access GatewayReadAccess, gateway, repository uuid.UUID, password []byte,
	run func(context.Context, string, string, string) error) (result error) {
	if !domain.ValidGatewayOrigin(access.Origin) || !access.Binding.Valid() || access.Binding.RepositoryID != repository {
		return ErrInvalidProvision
	}
	certificate, err := tls.X509KeyPair(access.CertificatePEM, access.PrivateKeyPEM)
	if err != nil || certificate.Leaf == nil {
		return ErrInvalidProvision
	}
	scope, err := security.GatewayReaderFromCertificate(certificate.Leaf)
	if err != nil || scope.RepositoryID != repository || scope.OperationID != access.Binding.OperationID || scope.AdmissionID != access.Binding.AdmissionID || scope.RuntimeID != access.Binding.RuntimeID {
		return ErrInvalidProvision
	}
	roots := x509.NewCertPool()
	if !domain.ValidCredentialCA(access.CABundlePEM) || !roots.AppendCertsFromPEM(access.CABundlePEM) {
		return ErrInvalidProvision
	}
	deadline := time.Now().Add(5 * time.Minute)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if certificate.Leaf.NotAfter.Before(deadline) {
		deadline = certificate.Leaf.NotAfter
	}
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	transport := &http.Transport{DialContext: (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots, Certificates: []tls.Certificate{certificate}},
		TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 25 * time.Second, MaxResponseHeaderBytes: 16 << 10, MaxConnsPerHost: 2, DisableCompression: true}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return ErrBackendFailed }}
	control := access.Origin + "/gateway/v1/repositories/" + repository.String() + "/read-sessions/" + access.Binding.OperationID.String()
	requestBody, _ := json.Marshal(struct {
		Deadline time.Time `json:"deadline"`
	}{deadline.UTC()})
	cleanupRequired := false
	defer func() {
		if !cleanupRequired {
			return
		}
		cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), 25*time.Second)
		defer stop()
		request, err := http.NewRequestWithContext(cleanup, http.MethodDelete, control, nil)
		if err != nil {
			result = ErrBackendFailed
			return
		}
		response, err := client.Do(request)
		if err != nil {
			result = ErrBackendFailed
			return
		}
		_ = response.Body.Close()
		if response.StatusCode != http.StatusNoContent {
			result = ErrBackendFailed
		}
	}()
	method := http.MethodPost
	retryUntil := time.Now().Add(30 * time.Second)
	for {
		var body io.Reader
		if method == http.MethodPost {
			body = bytes.NewReader(requestBody)
		}
		request, err := http.NewRequestWithContext(ctx, method, control, body)
		if err != nil {
			return ErrBackendFailed
		}
		if method == http.MethodPost {
			request.Header.Set("Content-Type", "application/json")
		}
		cleanupRequired = true
		response, err := client.Do(request)
		if err != nil {
			return ErrBackendFailed
		}
		raw, readErr := io.ReadAll(io.LimitReader(response.Body, 4097))
		_ = response.Body.Close()
		if method == http.MethodPost && response.StatusCode == http.StatusConflict && response.Header.Get("Retry-After") == "1" {
			cleanupRequired = false
			if time.Until(retryUntil) <= time.Second {
				return ErrRepositoryLocked
			}
		} else {
			var session gatewayReadSession
			if readErr != nil || len(raw) > 4096 || (response.StatusCode != http.StatusOK && response.StatusCode != http.StatusAccepted) || json.Unmarshal(raw, &session) != nil ||
				session.SessionID.Version() != 7 || session.SessionID.Variant() != uuid.RFC4122 || !session.ExpiresAt.After(time.Now()) || session.ExpiresAt.After(deadline) {
				return ErrBackendFailed
			}
			if session.State == "ACTIVE" {
				prefix := "/restic/" + gateway.String() + "/" + repository.String() + "/" + session.SessionID.String() + "/"
				if session.EndpointPath != prefix {
					return ErrBackendFailed
				}
				work, stop := context.WithDeadline(ctx, session.ExpiresAt)
				defer stop()
				return withGatewayReadFiles(access, password, func(dir, passwordFile string) error {
					return run(work, dir, "rest:"+access.Origin+prefix, passwordFile)
				})
			}
			if session.State != "STARTING" {
				return ErrBackendFailed
			}
			method = http.MethodGet
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func withGatewayReadFiles(access GatewayReadAccess, password []byte, run func(string, string) error) error {
	var fs syscall.Statfs_t
	if syscall.Statfs("/dev/shm", &fs) != nil || uint64(fs.Type) != 0x01021994 {
		return ErrInvalidProvision
	}
	directory, err := os.MkdirTemp("/dev/shm", "restfleet-read-")
	if err != nil {
		return ErrBackendFailed
	}
	defer func() { _ = os.RemoveAll(directory) }()
	if os.Chmod(directory, 0700) != nil {
		return ErrBackendFailed
	}
	client := make([]byte, 0, len(access.CertificatePEM)+len(access.PrivateKeyPEM))
	client = append(client, access.CertificatePEM...)
	client = append(client, access.PrivateKeyPEM...)
	defer clear(client)
	for _, file := range []struct {
		name string
		data []byte
	}{{"password", password}, {"client.pem", client}, {"ca.pem", access.CABundlePEM}} {
		path := filepath.Join(directory, file.name)
		handle, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
		if err != nil {
			return ErrBackendFailed
		}
		_, writeErr := handle.Write(file.data)
		closeErr := handle.Close()
		if writeErr != nil || closeErr != nil {
			return ErrBackendFailed
		}
	}
	return run(directory, filepath.Join(directory, "password"))
}
