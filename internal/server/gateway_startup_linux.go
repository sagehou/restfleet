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

	"github.com/google/uuid"
	"github.com/sagehou/restfleet/internal/domain"
	"github.com/sagehou/restfleet/internal/gatewaypending"
	"github.com/sagehou/restfleet/internal/security"
)

var ErrGatewayStartup = errors.New("gateway initialization unavailable or inconsistent")

// GatewayStartupConfig is CENTRAL-only trusted metadata, never an Agent request
// or an automatic restart recipe. RuntimeID identifies an independently
// provisioned fresh Gateway process; the source pin is its exported PUBLIC key.
type GatewayStartupConfig struct {
	Version         int                                  `json:"version"`
	Binding         security.GatewayAuthorizationBinding `json:"binding"`
	SourcePublic    ed25519.PublicKey                    `json:"source_public"`
	DecisionID      uuid.UUID                            `json:"decision_id"`
	LifetimeSeconds int64                                `json:"lifetime_seconds"`
	SocketPath      string                               `json:"socket_path"`
	GatewayUID      uint32                               `json:"gateway_uid"`
	SharedGroup     uint32                               `json:"shared_group"`
}

func (s GatewayStartupConfig) Validate() error {
	if s.Version != 1 || s.Binding.Validate() != nil || len(s.SourcePublic) != ed25519.PublicKeySize ||
		s.LifetimeSeconds < 1 || s.LifetimeSeconds > security.MaxGatewayAuthorizationSeconds ||
		!filepath.IsAbs(s.SocketPath) || filepath.Clean(s.SocketPath) != s.SocketPath || len(s.SocketPath) > 107 || strings.IndexByte(s.SocketPath, 0) >= 0 ||
		s.GatewayUID == ^uint32(0) || s.SharedGroup == ^uint32(0) {
		return ErrGatewayStartup
	}
	uid := uint32(os.Geteuid())
	if (s.SharedGroup == 0 && s.GatewayUID != uid) ||
		(s.SharedGroup != 0 && (uid == 0 || s.GatewayUID == 0 || s.GatewayUID == uid)) {
		return ErrGatewayStartup
	}
	if s.decision().Validate() != nil {
		return ErrGatewayStartup
	}
	return nil
}

func (s GatewayStartupConfig) decision() domain.GatewayDecisionRequest {
	return domain.GatewayDecisionRequest{ID: s.DecisionID, AdmissionID: s.Binding.AdmissionID, Owner: s.Binding.Owner,
		RuntimeID: s.Binding.RuntimeID, Lifetime: time.Duration(s.LifetimeSeconds) * time.Second}
}

// LoadGatewayStartupConfig accepts the documented field order/encoding, with
// optional whitespace. Exact re-encoding rejects duplicate, unknown, omitted,
// case-folded, null and alternative representations rather than normalizing.
func LoadGatewayStartupConfig(path string) (GatewayStartupConfig, error) {
	raw, err := security.ReadProtectedGatewayFile(path, 4096)
	if err != nil {
		return GatewayStartupConfig{}, ErrGatewayStartup
	}
	defer clear(raw)
	var s GatewayStartupConfig
	if json.Unmarshal(raw, &s) != nil || s.Validate() != nil {
		return GatewayStartupConfig{}, ErrGatewayStartup
	}
	canonical, err := json.Marshal(s)
	var compact bytes.Buffer
	if err != nil || json.Compact(&compact, raw) != nil || !bytes.Equal(canonical, compact.Bytes()) {
		return GatewayStartupConfig{}, ErrGatewayStartup
	}
	return s, nil
}

// InitializeGateway authenticates the pinned source/UID BEFORE first grant,
// registration and the existing one-intent encrypted material transaction.
// This is a single bounded attempt; it neither spawns processes nor renews,
// seals, releases, retries or declares readiness. Serialize it with all other
// decisions/delivery/cleanup for this admission in the trusted coordinator.
func (c *ControlPlane) InitializeGateway(ctx context.Context, s GatewayStartupConfig) error {
	if s.Validate() != nil || s.Binding.ConfigurationHash != c.credentialConfigurationHash() ||
		len(c.gatewaySigningKey) != ed25519.PrivateKeySize || len(c.gatewayPendingKey) != 32 || len(c.masterKey) != 32 {
		return ErrGatewayStartup
	}
	if _, ok := c.store.(GatewayPendingStore); !ok {
		return ErrGatewayStartup
	}
	if _, ok := c.store.(GatewayDecisionStore); !ok {
		return ErrGatewayStartup
	}
	if _, ok := c.store.(GatewayMaterialStore); !ok {
		return ErrGatewayStartup
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := c.Ready(ctx); err != nil || ctx.Err() != nil {
		return ErrGatewayStartup
	}
	s.SourcePublic = append(ed25519.PublicKey(nil), s.SourcePublic...)
	err := gatewaypending.SendMaterial(ctx, s.SocketPath, s.GatewayUID, s.Binding, s.SourcePublic,
		func(ctx context.Context, challenge []byte) ([]byte, error) {
			wire, err := c.DecideGatewayAuthorization(ctx, s.decision())
			if err != nil {
				return nil, ErrGatewayStartup
			}
			statement, err := security.VerifyGatewayStatement(wire, c.gatewaySigningKey.Public().(ed25519.PublicKey))
			if err != nil || statement.Binding != s.Binding || statement.Revision != 1 || statement.Revoked {
				return nil, ErrGatewayStartup
			}
			if _, _, err = c.RegisterGatewayPending(ctx, s.Binding, s.SourcePublic); err != nil {
				return nil, ErrGatewayStartup
			}
			return c.GatewayMaterialDelivery(ctx, s.Binding, s.SourcePublic, challenge)
		}, c.RecordGatewayMaterialDenied, s.SharedGroup)
	if err != nil {
		return ErrGatewayStartup
	}
	return nil
}
