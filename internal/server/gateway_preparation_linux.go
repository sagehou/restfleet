package server

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/sagehou/restfleet/internal/domain"
	"github.com/sagehou/restfleet/internal/security"
)

// GatewayPreparation is protected CENTRAL metadata for one repository in a
// freshly provisioned runtime. The runtime/source pins come from trusted local
// provisioning, never an Agent request. No private key is part of this input.
type GatewayPreparation struct {
	Version                  int               `json:"version"`
	AgentID                  uuid.UUID         `json:"agent_id"`
	RuntimeID                uuid.UUID         `json:"runtime_id"`
	SourcePublic             ed25519.PublicKey `json:"source_public"`
	AdmissionLifetimeSeconds int64             `json:"admission_lifetime_seconds"`
	LifetimeSeconds          int64             `json:"lifetime_seconds"`
	MaterialSocket           string            `json:"material_socket"`
	AuthoritySocket          string            `json:"authority_socket"`
	GatewayUID               uint32            `json:"gateway_uid"`
	SharedGroup              uint32            `json:"shared_group"`
}

func (p GatewayPreparation) valid() bool {
	if p.Version != 1 || p.AgentID.Version() != 7 || p.AgentID.Variant() != uuid.RFC4122 ||
		p.RuntimeID.Version() != 7 || p.RuntimeID.Variant() != uuid.RFC4122 ||
		p.AdmissionLifetimeSeconds < 60 || p.AdmissionLifetimeSeconds > int64(domain.MaxGatewayOfflineWindow/time.Second) ||
		p.LifetimeSeconds < 0 || p.LifetimeSeconds > security.MaxGatewayAuthorizationSeconds ||
		len(p.SourcePublic) != ed25519.PublicKeySize || bytes.Equal(p.SourcePublic, make([]byte, ed25519.PublicKeySize)) ||
		p.MaterialSocket == p.AuthoritySocket {
		return false
	}
	for _, socket := range []string{p.MaterialSocket, p.AuthoritySocket} {
		if !validGatewayAuthorityPeer(socket, p.GatewayUID, p.SharedGroup) {
			return false
		}
	}
	return true
}

func LoadGatewayPreparation(path string) (GatewayPreparation, error) {
	raw, err := security.ReadProtectedGatewayFile(path, 4096)
	if err != nil {
		return GatewayPreparation{}, ErrGatewayStartup
	}
	defer clear(raw)
	var p GatewayPreparation
	if json.Unmarshal(raw, &p) != nil || !p.valid() {
		return GatewayPreparation{}, ErrGatewayStartup
	}
	canonical, err := json.Marshal(p)
	var compact bytes.Buffer
	if err != nil || json.Compact(&compact, raw) != nil || !bytes.Equal(canonical, compact.Bytes()) {
		return GatewayPreparation{}, ErrGatewayStartup
	}
	return p, nil
}

// GatewayPreparedStartup is durable public metadata, not permission to run or
// recover a process. Save it BEFORE reserving, so a lost commit/response cannot
// lose the reservation identity or turn a retry into a fresh admission.
type GatewayPreparedStartup struct {
	Version     int                  `json:"version"`
	Preparation GatewayPreparation   `json:"preparation"`
	Startup     GatewayStartupConfig `json:"startup"`
}

func (p GatewayPreparedStartup) admission() domain.BackupAdmissionRequest {
	b := p.Startup.Binding
	return domain.BackupAdmissionRequest{ID: b.AdmissionID, Owner: b.Owner, AgentID: b.AgentID,
		DeliveryID: b.DeliveryID, ConfigurationHash: b.ConfigurationHash,
		Lifetime: time.Duration(p.Preparation.AdmissionLifetimeSeconds) * time.Second}
}

func decodeGatewayPreparedStartup(raw []byte) (GatewayPreparedStartup, error) {
	var p GatewayPreparedStartup
	if len(raw) > 8192 || json.Unmarshal(raw, &p) != nil || p.Version != 2 || !p.Preparation.valid() ||
		p.Startup.Validate() != nil || p.admission().Validate() != nil {
		return GatewayPreparedStartup{}, ErrGatewayStartup
	}
	s, r := p.Startup, p.Preparation
	if s.Binding.AgentID != r.AgentID || s.Binding.RuntimeID != r.RuntimeID || !bytes.Equal(s.SourcePublic, r.SourcePublic) ||
		s.SocketPath != r.MaterialSocket || s.GatewayUID != r.GatewayUID || s.SharedGroup != r.SharedGroup || s.LifetimeSeconds != r.LifetimeSeconds {
		return GatewayPreparedStartup{}, ErrGatewayStartup
	}
	canonical, err := json.Marshal(p)
	var compact bytes.Buffer
	if err != nil || json.Compact(&compact, raw) != nil || !bytes.Equal(canonical, compact.Bytes()) {
		return GatewayPreparedStartup{}, ErrGatewayStartup
	}
	return p, nil
}

// GatewayPreparationStore is deliberately separate from Store: provisioning
// needs accepted metadata and the existing admission transaction, not secrets.
type GatewayPreparationStore interface {
	AcceptedAgentCredential(context.Context, uuid.UUID, string) (domain.AgentCredentialDelivery, error)
	ReserveBackupAdmission(context.Context, domain.BackupAdmissionRequest) (domain.BackupAdmission, error)
}

func (c *ControlPlane) PrepareGateway(ctx context.Context, input GatewayPreparation, planPath string) error {
	store, ok := c.store.(GatewayPreparationStore)
	if !ok || !input.valid() || ctx.Err() != nil || c.gatewayPublicURL == "" ||
		input.LifetimeSeconds > int64(c.gatewayOfflineWindow/time.Second) ||
		len(c.gatewaySigningKey) != ed25519.PrivateKeySize || len(c.gatewayPendingKey) != 32 || len(c.masterKey) != 32 {
		return ErrGatewayStartup
	}
	if _, err := c.Ready(ctx); err != nil {
		return ErrGatewayStartup
	}
	d, err := store.AcceptedAgentCredential(ctx, input.AgentID, c.credentialConfigurationHash())
	if err != nil || d.AgentID != input.AgentID || d.AcceptedAt == nil {
		return ErrGatewayStartup
	}
	p := GatewayPreparedStartup{Version: 2, Preparation: input}
	var raw []byte
	if _, err := os.Lstat(planPath); err == nil {
		raw, err = security.ReadProtectedGatewayFile(planPath, 8192)
		if err != nil {
			return ErrGatewayStartup
		}
		defer clear(raw)
		p, err = decodeGatewayPreparedStartup(raw)
		if err != nil {
			return ErrGatewayStartup
		}
		expected, err := json.Marshal(input)
		actual, marshalErr := json.Marshal(p.Preparation)
		if err != nil || marshalErr != nil || !bytes.Equal(expected, actual) {
			return ErrGatewayStartup
		}
	} else if errors.Is(err, os.ErrNotExist) {
		ids := make([]uuid.UUID, 3)
		for i := range ids {
			ids[i], err = uuid.NewV7()
			if err != nil {
				return ErrGatewayStartup
			}
		}
		r := d.Repository
		p.Startup = GatewayStartupConfig{Version: 1, Binding: security.GatewayAuthorizationBinding{AdmissionID: ids[0], Owner: ids[1],
			RuntimeID: input.RuntimeID, AgentID: input.AgentID, HostID: r.HostID, RepositoryID: r.ID, GatewayID: r.GatewayID,
			StorageCredentialID: r.StorageCredentialID, DeliveryID: d.ID, GatewaySecretRef: r.GatewaySecretRef,
			ResticSecretRef: r.ResticSecretRef, ConfigurationHash: d.ConfigurationHash}, SourcePublic: bytes.Clone(input.SourcePublic),
			DecisionID: ids[2], LifetimeSeconds: input.LifetimeSeconds, SocketPath: input.MaterialSocket,
			GatewayUID: input.GatewayUID, SharedGroup: input.SharedGroup}
		raw, err = json.Marshal(p)
		if err != nil {
			return ErrGatewayStartup
		}
		if _, err = decodeGatewayPreparedStartup(raw); err != nil {
			return ErrGatewayStartup
		}
	} else {
		return ErrGatewayStartup
	}
	r, b := d.Repository, p.Startup.Binding
	if b.HostID != r.HostID || b.RepositoryID != r.ID || b.GatewayID != r.GatewayID || b.StorageCredentialID != r.StorageCredentialID ||
		b.DeliveryID != d.ID || b.GatewaySecretRef != r.GatewaySecretRef || b.ResticSecretRef != r.ResticSecretRef || b.ConfigurationHash != d.ConfigurationHash {
		return ErrGatewayStartup
	}
	if security.PersistProtectedGatewayFile(planPath, raw) != nil {
		return ErrGatewayStartup
	}
	a, err := store.ReserveBackupAdmission(ctx, p.admission())
	if err != nil || a.ReleasedAt != nil || pendingBinding(domain.GatewayPendingOrigin{Admission: a, RuntimeID: input.RuntimeID}) != b {
		return ErrGatewayStartup
	}
	return nil
}
