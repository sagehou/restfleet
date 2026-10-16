package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/sagehou/restfleet/internal/domain"
	"github.com/sagehou/restfleet/internal/security"
)

// RepositoryGatewayReader reads only authenticated, committed observations and
// current authority. It does not probe a backend or decrypt any secret.
type RepositoryGatewayReader interface {
	RepositoryGatewayObservations(context.Context, []uuid.UUID, string) (map[uuid.UUID]domain.RepositoryGatewayObservation, error)
}

func (c *ControlPlane) Repositories(ctx context.Context, after uuid.UUID, limit int) ([]domain.Repository, error) {
	if limit < 1 || limit > 201 {
		return nil, &ValidationError{Field: "limit", Code: "INVALID_LIMIT"}
	}
	items, err := c.store.Repositories(ctx, after, limit)
	if err != nil {
		return nil, err
	}
	return c.repositoryAvailability(ctx, items)
}

func (c *ControlPlane) Repository(ctx context.Context, id uuid.UUID) (domain.Repository, error) {
	r, err := c.store.Repository(ctx, id)
	if err != nil {
		return r, err
	}
	items, err := c.repositoryAvailability(ctx, []domain.Repository{r})
	if err != nil {
		return domain.Repository{}, err
	}
	return items[0], nil
}

func (c *ControlPlane) repositoryAvailability(ctx context.Context, items []domain.Repository) ([]domain.Repository, error) {
	if len(items) == 0 {
		return items, nil
	}
	var observations map[uuid.UUID]domain.RepositoryGatewayObservation
	if reader, ok := c.store.(RepositoryGatewayReader); ok {
		ids := make([]uuid.UUID, len(items))
		for i, item := range items {
			ids[i] = item.ID
		}
		var err error
		observations, err = reader.RepositoryGatewayObservations(ctx, ids, c.credentialConfigurationHash())
		if err != nil {
			return nil, err
		}
	}
	now := c.clock().UTC().Truncate(time.Second)
	for i, item := range items {
		items[i] = item.WithGatewayObservation(observations[item.ID], now)
	}
	return items, nil
}

func (c *ControlPlane) RepositoryCount(ctx context.Context) (int64, error) {
	return c.store.RepositoryCount(ctx)
}

func (c *ControlPlane) CreateRepository(ctx context.Context, name string, hostID, credentialID uuid.UUID, shared bool, actor domain.User, meta RequestMeta) (domain.Repository, error) {
	if actor.Role != domain.RoleAdmin {
		if err := c.RecordDenied(ctx, "REPOSITORY_CREATE", "REPOSITORY", "ROLE_DENIED", meta); err != nil {
			return domain.Repository{}, err
		}
		return domain.Repository{}, ErrForbidden
	}
	if len(c.masterKey) != 32 {
		return domain.Repository{}, domain.ErrStorageUnavailable
	}
	if shared {
		if err := c.RecordDenied(ctx, "REPOSITORY_CREATE", "REPOSITORY", "SHARED_REPOSITORY_NOT_SUPPORTED", meta); err != nil {
			return domain.Repository{}, err
		}
		return domain.Repository{}, domain.ErrSharedRepository
	}
	name = strings.TrimSpace(name)
	if !utf8.ValidString(name) || utf8.RuneCountInString(name) < 1 || utf8.RuneCountInString(name) > 128 || strings.IndexFunc(name, unicode.IsControl) >= 0 {
		return domain.Repository{}, &ValidationError{Field: "name", Code: "INVALID_NAME"}
	}
	for field, id := range map[string]uuid.UUID{"host_id": hostID, "storage_credential_id": credentialID} {
		if id.Version() != 7 || id.Variant() != uuid.RFC4122 {
			return domain.Repository{}, &ValidationError{Field: field, Code: "INVALID_ID"}
		}
	}
	id, err := uuid.NewV7()
	if err != nil {
		return domain.Repository{}, err
	}
	gateway, err := uuid.NewV7()
	if err != nil {
		return domain.Repository{}, err
	}
	now := c.clock().UTC()
	repo := domain.Repository{ID: id, HostID: hostID, StorageCredentialID: credentialID, Name: name,
		GatewayID: gateway, BackendPath: "restfleet/agents/" + gateway.String() + "/" + id.String(),
		Status: "PROVISIONING", GatewaySecretRevision: 1, ResticSecretRevision: 1, Revision: 1,
		CreatedAt: now, UpdatedAt: now}
	gatewaySecret, err := c.sealRepositoryPassword(repo, "GATEWAY")
	if err != nil {
		return domain.Repository{}, err
	}
	resticSecret, err := c.sealRepositoryPassword(repo, "RESTIC_KEY")
	if err != nil {
		return domain.Repository{}, err
	}
	repo.GatewaySecretRef, repo.ResticSecretRef = gatewaySecret.ID, resticSecret.ID
	audit, err := c.userAudit("REPOSITORY_CREATE", "REPOSITORY", id, actor.ID, meta, "PROVISIONING")
	if err != nil {
		return domain.Repository{}, err
	}
	audit.Changes = json.RawMessage(`{"gateway_credential":{"changed":true,"revision":1},"restic_credential":{"changed":true,"revision":1}}`)
	return c.store.CreateRepository(ctx, repo, gatewaySecret, resticSecret, audit)
}

func repositoryAAD(repo domain.Repository, kind string, revision int64, secretID uuid.UUID) []byte {
	return []byte(fmt.Sprintf("restfleet:repository:v1:%s:%s:%s:%d:%s:master:v1", repo.HostID, repo.ID, kind, revision, secretID))
}

// Each kind is generated independently. No password is returned to the browser,
// and creation does not decrypt the referenced cloud-storage credential.
func (c *ControlPlane) sealRepositoryPassword(repo domain.Repository, kind string) (domain.SecretEnvelope, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return domain.SecretEnvelope{}, err
	}
	password, err := security.NewOpaqueToken()
	if err != nil {
		return domain.SecretEnvelope{}, err
	}
	raw := []byte(password)
	defer clear(raw)
	sealed, err := security.SealEnvelope(c.masterKey, raw, repositoryAAD(repo, kind, 1, id))
	if err != nil {
		return domain.SecretEnvelope{}, err
	}
	return domain.SecretEnvelope{ID: id, Kind: "REPOSITORY_" + kind, Algorithm: security.EnvelopeAlgorithm, KeyID: "master:v1",
		Ciphertext: sealed.Ciphertext, Nonce: sealed.Nonce, WrappedDataKey: sealed.WrappedDataKey,
		WrapNonce: sealed.WrapNonce, AAD: sealed.AAD, CreatedAt: repo.CreatedAt}, nil
}

// openRepositorySecret is reserved for audited central workers. Binding to both
// Host and Repository prevents a valid envelope from being substituted elsewhere.
func openRepositorySecret(key []byte, repo domain.Repository, kind string, e domain.SecretEnvelope) ([]byte, error) {
	ref, revision := repo.GatewaySecretRef, repo.GatewaySecretRevision
	switch kind {
	case "GATEWAY":
	case "RESTIC_KEY":
		ref, revision = repo.ResticSecretRef, repo.ResticSecretRevision
	default:
		return nil, domain.ErrStorageUnavailable
	}
	if e.ID != ref || e.Kind != "REPOSITORY_"+kind || e.KeyID != "master:v1" ||
		len(e.Nonce) != 12 || len(e.WrapNonce) != 12 ||
		e.Algorithm != security.EnvelopeAlgorithm || !bytes.Equal(e.AAD, repositoryAAD(repo, kind, revision, e.ID)) {
		return nil, domain.ErrStorageUnavailable
	}
	raw, err := security.OpenEnvelope(key, security.Envelope{Ciphertext: e.Ciphertext, Nonce: e.Nonce, WrappedDataKey: e.WrappedDataKey, WrapNonce: e.WrapNonce, AAD: e.AAD})
	if err != nil {
		return nil, domain.ErrStorageUnavailable
	}
	return raw, nil
}
