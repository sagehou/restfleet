package server

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/sagehou/restfleet/internal/domain"
	"github.com/sagehou/restfleet/internal/security"
)

// GatewayAuthorityTarget is protected CENTRAL routing metadata for an already
// initialized runtime. It contains no Agent secret or process restart recipe.
type GatewayAuthorityTarget struct {
	Binding      security.GatewayAuthorizationBinding `json:"binding"`
	SourcePublic ed25519.PublicKey                    `json:"source_public"`
	SocketPath   string                               `json:"socket_path"`
	GatewayUID   uint32                               `json:"gateway_uid"`
	SharedGroup  uint32                               `json:"shared_group"`
}

func (t GatewayAuthorityTarget) valid() bool {
	uid := uint32(os.Geteuid())
	return t.Binding.Validate() == nil && len(t.SourcePublic) == ed25519.PublicKeySize &&
		filepath.IsAbs(t.SocketPath) && filepath.Clean(t.SocketPath) == t.SocketPath && len(t.SocketPath) <= 107 &&
		strings.IndexByte(t.SocketPath, 0) < 0 && t.GatewayUID != ^uint32(0) && t.SharedGroup != ^uint32(0) &&
		((t.SharedGroup == 0 && t.GatewayUID == uid) ||
			(t.SharedGroup != 0 && uid != 0 && t.GatewayUID != 0 && t.GatewayUID != uid))
}

// LoadGatewayAuthorityTargets accepts canonical fields/encoding, as startup
// metadata does. Unknown/duplicate/omitted fields cannot choose alternate pins.
func LoadGatewayAuthorityTargets(path string) ([]GatewayAuthorityTarget, error) {
	raw, err := security.ReadProtectedGatewayFile(path, 64<<10)
	if err != nil {
		return nil, security.ErrGatewayAuthority
	}
	defer clear(raw)
	var config struct {
		Version int                      `json:"version"`
		Targets []GatewayAuthorityTarget `json:"targets"`
	}
	if json.Unmarshal(raw, &config) != nil || config.Version != 1 || len(config.Targets) == 0 || len(config.Targets) > 32 {
		return nil, security.ErrGatewayAuthority
	}
	canonical, err := json.Marshal(config)
	var compact bytes.Buffer
	if err != nil || json.Compact(&compact, raw) != nil || !bytes.Equal(canonical, compact.Bytes()) {
		return nil, security.ErrGatewayAuthority
	}
	seen := make(map[uuid.UUID]bool, len(config.Targets))
	for _, target := range config.Targets {
		if !target.valid() || seen[target.Binding.AdmissionID] {
			return nil, security.ErrGatewayAuthority
		}
		seen[target.Binding.AdmissionID] = true
	}
	return config.Targets, nil
}

// RunGatewayAuthorityWorker must be the sole coordinator for these admissions.
// PostgreSQL decisions are authoritative; acknowledged revisions only suppress
// redundant delivery. Restart/lost receipt first replays the exact DB decision.
// This worker never initializes, seals, releases or reconstructs a failed owner.
func (c *ControlPlane) RunGatewayAuthorityWorker(ctx context.Context, targets []GatewayAuthorityTarget, onRetry func()) error {
	if _, ok := c.store.(GatewayDecisionReader); !ok || len(targets) == 0 || len(targets) > 32 {
		return security.ErrGatewayAuthority
	}
	seen := make(map[uuid.UUID]bool, len(targets))
	for _, target := range targets {
		if !target.valid() || seen[target.Binding.AdmissionID] {
			return security.ErrGatewayAuthority
		}
		seen[target.Binding.AdmissionID] = true
	}
	var workers sync.WaitGroup
	for _, target := range targets {
		target.SourcePublic = append(ed25519.PublicKey(nil), target.SourcePublic...)
		workers.Add(1)
		go func() {
			defer workers.Done()
			ticker := time.NewTicker(10 * time.Second)
			defer ticker.Stop()
			var acknowledged int64
			for {
				if ctx.Err() != nil {
					return
				}
				attempt, cancel := context.WithTimeout(ctx, 10*time.Second)
				next, err := c.coordinateGatewayAuthority(attempt, target, acknowledged)
				cancel()
				if err == nil {
					acknowledged = next
				} else if ctx.Err() == nil && onRetry != nil {
					onRetry()
				}
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				}
			}
		}()
	}
	workers.Wait()
	return nil
}

func (c *ControlPlane) coordinateGatewayAuthority(ctx context.Context, t GatewayAuthorityTarget, acknowledged int64) (int64, error) {
	d, err := c.store.(GatewayDecisionReader).LatestGatewayAuthorization(ctx, t.Binding.AdmissionID, t.Binding.RuntimeID)
	if err != nil || d.Revision < 1 || pendingBinding(domain.GatewayPendingOrigin{Admission: d.Admission, RuntimeID: d.RuntimeID}) != t.Binding ||
		d.Admission.ReleasedAt != nil || d.IssuedAt.After(c.clock()) {
		return acknowledged, security.ErrGatewayAuthority
	}
	if d.Revoked && acknowledged == d.Revision {
		return acknowledged, nil
	}
	revoke := false
	if !d.Revoked {
		revoke, err = c.gatewayPrincipalWithdrawn(ctx, t.Binding)
		if err != nil {
			return acknowledged, err
		}
	}
	// Explicit withdrawal can supersede an uncertain grant; uncertainty must
	// never delay revocation, but it must prevent minting another live grant.
	if !revoke && acknowledged != d.Revision {
		err = c.ReplayGatewayAuthorization(ctx, t.SocketPath, t.GatewayUID, t.Binding, t.SourcePublic, d.Revision, t.SharedGroup)
		return d.Revision, err
	}
	if !revoke {
		now := c.clock()
		if !d.ExpiresAt.After(now) {
			return acknowledged, security.ErrGatewayAuthority
		}
		// Renew halfway through the actual grant, bounded by the original
		// admission. A shorter configured window affects only NEW decisions.
		if !d.ExpiresAt.Before(d.Admission.ExpiresAt.Truncate(time.Second)) ||
			now.Before(d.IssuedAt.Add(d.ExpiresAt.Sub(d.IssuedAt)/2)) {
			return acknowledged, nil
		}
	}
	id, err := uuid.NewV7()
	if err != nil {
		return acknowledged, security.ErrGatewayAuthority
	}
	r := domain.GatewayDecisionRequest{ID: id, AdmissionID: t.Binding.AdmissionID, Owner: t.Binding.Owner,
		RuntimeID: t.Binding.RuntimeID, ExpectedRevision: d.Revision, Revoke: revoke}
	if !revoke {
		r.Lifetime = c.gatewayOfflineWindow
	}
	err = c.DeliverGatewayAuthorization(ctx, t.SocketPath, t.GatewayUID, t.Binding, t.SourcePublic, r, t.SharedGroup)
	return d.Revision + 1, err
}

// Only explicit stored withdrawal becomes revocation. Offline health, expired
// certificates/grants, read errors and unavailable sockets never do.
func (c *ControlPlane) gatewayPrincipalWithdrawn(ctx context.Context, b security.GatewayAuthorizationBinding) (bool, error) {
	a, err := c.store.Agent(ctx, b.AgentID)
	if err != nil {
		return false, security.ErrGatewayAuthority
	}
	if a.Status == domain.AgentRevoked {
		return true, nil
	}
	h, err := c.store.Host(ctx, b.HostID)
	if err != nil {
		return false, security.ErrGatewayAuthority
	}
	if h.Status == domain.HostDisabled || h.Status == domain.HostRevoked {
		return true, nil
	}
	r, err := c.store.Repository(ctx, b.RepositoryID)
	if err != nil {
		return false, security.ErrGatewayAuthority
	}
	if r.Status == "DISABLED" {
		return true, nil
	}
	s, err := c.store.StorageCredential(ctx, b.StorageCredentialID)
	if err != nil {
		return false, security.ErrGatewayAuthority
	}
	return s.Status == "DISABLED", nil
}
