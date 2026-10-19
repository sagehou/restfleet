package server

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/sagehou/restfleet/internal/domain"
	"github.com/sagehou/restfleet/internal/security"
)

var (
	ErrGatewayRotation = errors.New("gateway rotation unavailable or incomplete")
	ErrGatewayRecovery = errors.New("gateway recovery unavailable or inconsistent")
)

// GatewayRotationConfig is protected CENTRAL metadata for one normal runtime
// replacement. It never contains Gateway private material. The deployer MUST
// independently provision and start the next fresh runtime.
type GatewayRotationConfig struct {
	Version         int                `json:"version"`
	Current         GatewayAuthorityTarget `json:"current"`
	Next            GatewayPreparation `json:"next_preparation"`
	PlanFile        string             `json:"plan_file"`
	AuditConfigFile string             `json:"audit_config_file"`
	WaitSeconds     int                `json:"wait_seconds"`
}

func (r GatewayRotationConfig) valid(path string) bool {
	if r.Version != 1 || !r.Current.valid() || !r.Next.valid() || r.WaitSeconds < 1 || r.WaitSeconds > 300 ||
		r.Current.Binding.RuntimeID == r.Next.RuntimeID || r.Current.Binding.AgentID != r.Next.AgentID ||
		r.Current.GatewayUID != r.Next.GatewayUID || r.Current.SharedGroup != r.Next.SharedGroup ||
		r.PlanFile == r.AuditConfigFile || r.PlanFile == path || r.AuditConfigFile == path {
		return false
	}
	for _, p := range []string{r.PlanFile, r.AuditConfigFile} {
		if !filepath.IsAbs(p) || filepath.Clean(p) != p || p == "/" || strings.IndexFunc(p, unicode.IsControl) >= 0 {
			return false
		}
	}
	return true
}

// LoadGatewayRotationConfig uses the same protected file and canonical JSON
// policy as the existing startup/registration commands.
func LoadGatewayRotationConfig(path string) (GatewayRotationConfig, error) {
	raw, err := security.ReadProtectedGatewayFile(path, 16<<10)
	if err != nil {
		return GatewayRotationConfig{}, ErrGatewayRotation
	}
	defer clear(raw)
	var r GatewayRotationConfig
	if json.Unmarshal(raw, &r) != nil || !r.valid(path) {
		return GatewayRotationConfig{}, ErrGatewayRotation
	}
	canonical, err := json.Marshal(r)
	var compact bytes.Buffer
	if err != nil || json.Compact(&compact, raw) != nil || !bytes.Equal(canonical, compact.Bytes()) {
		return GatewayRotationConfig{}, ErrGatewayRotation
	}
	return r, nil
}

// GatewayRecoveryConfig is a protected operator decision for an expired
// admission without terminal cleanup. SourcePublic is optional only when the
// runtime never registered an origin.
type GatewayRecoveryConfig struct {
	Version      int                                  `json:"version"`
	Binding      security.GatewayAuthorizationBinding `json:"binding"`
	SourcePublic ed25519.PublicKey                    `json:"source_public,omitempty"`
}

func (r GatewayRecoveryConfig) valid() bool {
	if r.Version != 1 || r.Binding.Validate() != nil {
		return false
	}
	if len(r.SourcePublic) == 0 {
		return true
	}
	return len(r.SourcePublic) == ed25519.PublicKeySize && !bytes.Equal(r.SourcePublic, make([]byte, ed25519.PublicKeySize))
}

func LoadGatewayRecoveryConfig(path string) (GatewayRecoveryConfig, error) {
	raw, err := security.ReadProtectedGatewayFile(path, 4096)
	if err != nil {
		return GatewayRecoveryConfig{}, ErrGatewayRecovery
	}
	defer clear(raw)
	var r GatewayRecoveryConfig
	if json.Unmarshal(raw, &r) != nil || !r.valid() {
		return GatewayRecoveryConfig{}, ErrGatewayRecovery
	}
	canonical, err := json.Marshal(r)
	var compact bytes.Buffer
	if err != nil || json.Compact(&compact, raw) != nil || !bytes.Equal(canonical, compact.Bytes()) {
		return GatewayRecoveryConfig{}, ErrGatewayRecovery
	}
	return r, nil
}

// GatewayRotationStore adds only the terminal-record proof and retains the
// existing preparation/decision ports. No schema or new owner identity.
type GatewayRotationStore interface {
	GatewayPreparationStore
	GatewayDecisionReader
	GatewayPendingStore
	GatewayRuntimeCleanupComplete(context.Context, uuid.UUID, uuid.UUID) (bool, error)
}

type GatewayRecoveryStore interface {
	RecoverGatewayAdmission(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, ed25519.PublicKey) error
}

// RotateGateway is the sole central coordinator for a normal replacement.
// It explicitly withdraws the old runtime, waits for the signed terminal
// cleanup record and its atomic release, and only then reserves the next
// admission. A missing proof leaves the old fence in place. The next process
// is still started by the trusted deployment, never by this command.
func (c *ControlPlane) RotateGateway(ctx context.Context, r GatewayRotationConfig, onRetry func()) error {
	store, ok := c.store.(GatewayRotationStore)
	if !ok || !r.valid("") || len(c.gatewaySigningKey) != ed25519.PrivateKeySize ||
		len(c.gatewayPendingKey) != 32 || len(c.masterKey) != 32 || ctx.Err() != nil {
		return ErrGatewayRotation
	}
	read := func() (domain.GatewayDecision, error) {
		readCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		return store.LatestGatewayAuthorization(readCtx, r.Current.Binding.AdmissionID, r.Current.Binding.RuntimeID)
	}
	d, err := read()
	if err != nil || d.Admission.AgentID != r.Next.AgentID ||
		pendingBinding(domain.GatewayPendingOrigin{Admission: d.Admission, RuntimeID: d.RuntimeID}) != r.Current.Binding {
		return ErrGatewayRotation
	}
	if d.Admission.ReleasedAt == nil {
		now := c.clock()
		reserve := time.Duration(r.WaitSeconds+60) * time.Second
		if !now.Before(d.Admission.ExpiresAt) || d.Admission.ExpiresAt.Sub(now) <= reserve {
			return ErrGatewayRotation
		}
		if !d.Revoked {
			id, idErr := uuid.NewV7()
			if idErr != nil {
				return ErrGatewayRotation
			}
			decision := domain.GatewayDecisionRequest{ID: id, AdmissionID: r.Current.Binding.AdmissionID,
				Owner: r.Current.Binding.Owner, RuntimeID: r.Current.Binding.RuntimeID,
				ExpectedRevision: d.Revision, Revoke: true}
			deliverErr := c.DeliverGatewayAuthorization(ctx, r.Current.SocketPath, r.Current.GatewayUID,
				r.Current.Binding, r.Current.SourcePublic, decision, r.Current.SharedGroup)
			if deliverErr != nil {
				// The decision transaction may have committed before delivery
				// failed. Only the exact committed revision permits waiting.
				latest, readErr := read()
				if readErr != nil || !latest.Revoked || latest.Revision != d.Revision+1 {
					return ErrGatewayRotation
				}
				d = latest
			} else {
				d.Revision++
				d.Revoked = true
			}
		}
		if d.Revoked {
			// Exact replay is idempotent. A failed replay may mean the runtime is
			// already stopping; the release proof remains authoritative.
			_ = c.ReplayGatewayAuthorization(ctx, r.Current.SocketPath, r.Current.GatewayUID,
				r.Current.Binding, r.Current.SourcePublic, d.Revision, r.Current.SharedGroup)
		}
		waitCtx, cancel := context.WithTimeout(ctx, time.Duration(r.WaitSeconds)*time.Second)
		defer cancel()
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			d, err = read()
			if err != nil {
				return ErrGatewayRotation
			}
			if d.Admission.ReleasedAt != nil {
				break
			}
			select {
			case <-waitCtx.Done():
				return ErrGatewayRotation
			case <-ticker.C:
			}
		}
	}
	terminal, err := store.GatewayRuntimeCleanupComplete(ctx, r.Current.Binding.AdmissionID, r.Current.Binding.RuntimeID)
	if err != nil || !terminal {
		return ErrGatewayRotation
	}
	prepareCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if c.PrepareGateway(prepareCtx, r.Next, r.PlanFile) != nil {
		return ErrGatewayRotation
	}
	audit, err := LoadGatewayAuditRegistrationConfig(r.AuditConfigFile)
	if err != nil || audit.AuditOrigin.RuntimeID != r.Next.RuntimeID || bytes.Equal(audit.SourcePublic, r.Next.SourcePublic) {
		return ErrGatewayRotation
	}
	registerCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if _, err = c.RegisterGatewayAuditFromConfig(registerCtx, audit); err != nil {
		return ErrGatewayRotation
	}
	startup, err := LoadGatewayStartupConfig(r.PlanFile)
	if err != nil || startup.Binding.RuntimeID != r.Next.RuntimeID || startup.Binding.AgentID != r.Next.AgentID ||
		startup.Binding.HostID != r.Current.Binding.HostID || startup.Binding.RepositoryID != r.Current.Binding.RepositoryID ||
		startup.Binding.GatewayID != r.Current.Binding.GatewayID ||
		startup.Binding.StorageCredentialID != r.Current.Binding.StorageCredentialID ||
		!bytes.Equal(startup.SourcePublic, r.Next.SourcePublic) || startup.SocketPath != r.Next.MaterialSocket ||
		startup.GatewayUID != r.Next.GatewayUID || startup.SharedGroup != r.Next.SharedGroup {
		return ErrGatewayRotation
	}
	socketCtx, cancel := context.WithTimeout(ctx, time.Duration(r.WaitSeconds)*time.Second)
	defer cancel()
	if waitRotationMaterialSocket(socketCtx, startup.SocketPath) != nil {
		return ErrGatewayRotation
	}
	if c.InitializeGateway(ctx, startup) != nil {
		// A retry may find the one-shot initialization already registered even
		// though the receipt was lost. Only the existing origin permits handing
		// off to the worker; it never re-sends material or initializes again.
		origin, originErr := store.GatewayPendingOrigin(ctx, startup.Binding.AdmissionID, startup.Binding.RuntimeID)
		if originErr != nil || origin.ClosedAt != nil {
			return ErrGatewayRotation
		}
	}
	targets, err := LoadGatewayAuthorityTargets(r.PlanFile)
	if err != nil || len(targets) != 1 || targets[0].Binding != startup.Binding ||
		!bytes.Equal(targets[0].SourcePublic, startup.SourcePublic) || targets[0].GatewayUID != startup.GatewayUID ||
		targets[0].SharedGroup != startup.SharedGroup || targets[0].SocketPath == startup.SocketPath {
		return ErrGatewayRotation
	}
	return c.RunGatewayAuthorityWorker(ctx, targets, onRetry)
}

// RecoverGateway is CENTRAL-ONLY and deliberately rejects a live admission.
// The operator attests that the old host/process was fenced; the store records
// that decision, revokes/seals/releases atomically and never fabricates the
// signed cleanup record.
func (c *ControlPlane) RecoverGateway(ctx context.Context, r GatewayRecoveryConfig) error {
	store, ok := c.store.(GatewayRecoveryStore)
	if !ok || !r.valid() || len(c.gatewaySigningKey) != ed25519.PrivateKeySize ||
		len(c.gatewayPendingKey) != 32 || len(c.masterKey) != 32 {
		return ErrGatewayRecovery
	}
	recoverCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := c.Ready(recoverCtx); err != nil || store.RecoverGatewayAdmission(recoverCtx,
		r.Binding.AdmissionID, r.Binding.Owner, r.Binding.RuntimeID, r.SourcePublic) != nil {
		return ErrGatewayRecovery
	}
	return nil
}

func waitRotationMaterialSocket(ctx context.Context, path string) error {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return ErrGatewayRotation
		}
		info, err := os.Lstat(path)
		if err == nil {
			if info.Mode()&os.ModeSocket == 0 {
				return ErrGatewayRotation
			}
			return nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return ErrGatewayRotation
		}
		select {
		case <-ctx.Done():
			return ErrGatewayRotation
		case <-ticker.C:
		}
	}
}
