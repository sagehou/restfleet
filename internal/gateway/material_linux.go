package gateway

import (
	"crypto/ed25519"
	"time"

	"github.com/sagehou/restfleet/internal/domain"
	"github.com/sagehou/restfleet/internal/gatewaypending"
	"github.com/sagehou/restfleet/internal/rclone"
	"github.com/sagehou/restfleet/internal/security"
)

// InstallGatewayMaterial is solely for MaterialReceiver's authenticated callback,
// never a deserialized HTTP/gRPC request. It creates a NEW encrypted queue and
// one local lifecycle owner. The caller owns the returned queue and MUST retain
// it for replay if receipt delivery fails; failure never authorizes takeover.
func InstallGatewayMaterial(supervisor *Supervisor, path string, limits gatewaypending.Limits,
	material security.GatewayMaterial, source ed25519.PrivateKey, central ed25519.PublicKey,
) (*AuthorizedBackup, *gatewaypending.Queue, error) {
	if supervisor == nil || len(source) != 64 || material.Validate(central) != nil ||
		!ed25519.PublicKey(material.Source).Equal(source.Public()) {
		return nil, nil, security.ErrGatewayMaterial
	}
	config, err := rclone.ParseConfig(string(material.Config), material.Remote)
	if err != nil {
		return nil, nil, security.ErrGatewayMaterial
	}
	raw := config.Bytes()
	defer clear(raw)
	if security.GatewayPendingHash(raw) != material.ConfigHash {
		return nil, nil, security.ErrGatewayMaterial
	}
	b := material.Challenge.Binding
	authorization, err := NewAuthorization(central, b)
	if err != nil || authorization.Accept(material.Statement) != nil || authorization.Status() != AuthorizationValid {
		return nil, nil, security.ErrGatewayMaterial
	}
	q, err := gatewaypending.Create(path, b, material.PendingRecipient, source, central, limits)
	if err != nil {
		return nil, nil, security.ErrGatewayMaterial
	}
	origin := domain.GatewayPendingOrigin{Admission: domain.BackupAdmission{ID: b.AdmissionID, Owner: b.Owner, AgentID: b.AgentID,
		HostID: b.HostID, RepositoryID: b.RepositoryID, GatewayID: b.GatewayID, StorageCredentialID: b.StorageCredentialID,
		DeliveryID: b.DeliveryID, GatewaySecretRef: b.GatewaySecretRef, ResticSecretRef: b.ResticSecretRef, ConfigurationHash: b.ConfigurationHash,
		CreatedAt: time.Unix(material.AdmissionCreatedAt, 0).UTC(), ExpiresAt: time.Unix(material.AdmissionExpiresAt, 0).UTC()},
		RuntimeID: b.RuntimeID, PublicKey: append(ed25519.PublicKey(nil), material.Source...), InitialSecretRevision: material.SecretRevision}
	owner, err := NewAuthorizedBackup(supervisor, authorization, q, origin, raw, material.Remote)
	if err != nil {
		q.Freeze()
		_ = q.Close()
		return nil, nil, security.ErrGatewayMaterial
	}
	return owner, q, nil
}
