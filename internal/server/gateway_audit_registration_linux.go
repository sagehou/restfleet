package server

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"time"

	"github.com/sagehou/restfleet/internal/domain"
	"github.com/sagehou/restfleet/internal/security"
)

var ErrGatewayAuditRegistration = errors.New("gateway audit registration unavailable or inconsistent")

// GatewayAuditRegistrationConfig is central-only trusted public metadata.
// It MUST be independently provisioned, never obtained from an Agent, replay
// frame or an unverified Gateway peer. Registration grants audit ingestion only.
type GatewayAuditRegistrationConfig struct {
	Version      int                        `json:"version"`
	AuditOrigin  domain.GatewayAuditBinding `json:"audit_origin"`
	SourcePublic ed25519.PublicKey          `json:"source_public"`
}

func (s GatewayAuditRegistrationConfig) Validate() error {
	if s.Version != 1 || s.AuditOrigin.Validate() != nil || len(s.SourcePublic) != ed25519.PublicKeySize ||
		bytes.Equal(s.SourcePublic, make([]byte, ed25519.PublicKeySize)) {
		return ErrGatewayAuditRegistration
	}
	return nil
}

// LoadGatewayAuditRegistrationConfig preserves the protected file policy and
// exact JSON encoding used by the existing initialization/replay commands.
func LoadGatewayAuditRegistrationConfig(path string) (GatewayAuditRegistrationConfig, error) {
	raw, err := security.ReadProtectedGatewayFile(path, 1024)
	if err != nil {
		return GatewayAuditRegistrationConfig{}, ErrGatewayAuditRegistration
	}
	defer clear(raw)
	var s GatewayAuditRegistrationConfig
	if json.Unmarshal(raw, &s) != nil || s.Validate() != nil {
		return GatewayAuditRegistrationConfig{}, ErrGatewayAuditRegistration
	}
	canonical, err := json.Marshal(s)
	var compact bytes.Buffer
	if err != nil || json.Compact(&compact, raw) != nil || !bytes.Equal(canonical, compact.Bytes()) {
		return GatewayAuditRegistrationConfig{}, ErrGatewayAuditRegistration
	}
	return s, nil
}

// GatewayAuditRegistration reports only a committed public audit-source
// registration. It proves neither process freshness, producer cleanup nor READY.
type GatewayAuditRegistration struct {
	Version         int                        `json:"version"`
	AuditOrigin     domain.GatewayAuditBinding `json:"audit_origin"`
	SourcePublic    ed25519.PublicKey          `json:"source_public"`
	RecipientPublic []byte                     `json:"recipient_public"`
	CreatedAt       time.Time                  `json:"created_at"`
}

func (c *ControlPlane) RegisterGatewayAuditFromConfig(ctx context.Context, s GatewayAuditRegistrationConfig) (GatewayAuditRegistration, error) {
	if s.Validate() != nil || ctx.Err() != nil {
		return GatewayAuditRegistration{}, ErrGatewayAuditRegistration
	}
	s.SourcePublic = bytes.Clone(s.SourcePublic)
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if _, err := c.Ready(ctx); err != nil || ctx.Err() != nil {
		return GatewayAuditRegistration{}, ErrGatewayAuditRegistration
	}
	o, recipient, err := c.RegisterGatewayGlobalAudit(ctx, s.AuditOrigin, s.SourcePublic)
	if err != nil || ctx.Err() != nil || o.CreatedAt.IsZero() {
		return GatewayAuditRegistration{}, ErrGatewayAuditRegistration
	}
	return GatewayAuditRegistration{Version: 1, AuditOrigin: o.Binding, SourcePublic: bytes.Clone(o.PublicKey),
		RecipientPublic: bytes.Clone(recipient[:]), CreatedAt: o.CreatedAt.UTC()}, nil
}
