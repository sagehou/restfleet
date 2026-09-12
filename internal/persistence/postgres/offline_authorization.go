package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/sagehou/restfleet/internal/domain"
)

const offlineAuthColumns = `id,owner,gateway_instance_id,agent_id,host_id,repository_id,gateway_id,
storage_credential_id,delivery_id,configuration_hash,authorized_at,expires_at,authorization_sequence,
renewed_at,revoked_at,disabled_at,created_at`

func scanOfflineAuth(row rowScanner) (domain.OfflineAuthorization, error) {
	var a domain.OfflineAuthorization
	err := row.Scan(&a.ID, &a.Owner, &a.GatewayInstanceID, &a.AgentID, &a.HostID, &a.RepositoryID,
		&a.GatewayID, &a.StorageCredentialID, &a.DeliveryID, &a.ConfigurationHash,
		&a.AuthorizedAt, &a.ExpiresAt, &a.AuthorizationSequence,
		&a.RenewedAt, &a.RevokedAt, &a.DisabledAt, &a.CreatedAt)
	return a, err
}

// IssueOfflineAuthorization creates a new bounded offline authorization grant.
// The trusted caller MUST verify the Agent/Host/Repository/Credential are all
// active and the current delivery ACK matches. The authorization is bound to a
// specific gateway instance and cannot be transferred.
func (s *Store) IssueOfflineAuthorization(ctx context.Context, request domain.OfflineAuthorizationRequest) (domain.OfflineAuthorization, error) {
	if err := request.Validate(); err != nil {
		return domain.OfflineAuthorization{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return domain.OfflineAuthorization{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	deny := func(cause error, repoID uuid.UUID) (domain.OfflineAuthorization, error) {
		return domain.OfflineAuthorization{}, rejectOfflineAuth(ctx, tx, request.ID, repoID, cause)
	}

	// Agent -> Host -> Repository -> credential -> authorization -> audit.
	r, err := lockAgentRepository(ctx, tx, request.AgentID, true)
	if errors.Is(err, domain.ErrNotFound) {
		return deny(domain.ErrOfflineAuthorization, uuid.Nil)
	}
	if err != nil {
		return domain.OfflineAuthorization{}, err
	}
	if r.Status == "LOCKED" || r.Status == "DISABLED" {
		return deny(domain.ErrOfflineAuthorization, r.ID)
	}

	d, err := scanAgentDelivery(ctx, tx, request.AgentID)
	if errors.Is(err, pgx.ErrNoRows) {
		return deny(domain.ErrOfflineAuthorization, r.ID)
	}
	if err != nil {
		return domain.OfflineAuthorization{}, err
	}
	if d.AcceptedAt == nil || d.ConfigurationHash != request.ConfigurationHash || d.Repository.ID != r.ID {
		return deny(domain.ErrOfflineAuthorization, r.ID)
	}

	if _, err = tx.Exec(ctx, "select pg_advisory_xact_lock(hashtextextended($1,0))", r.ID.String()); err != nil {
		return domain.OfflineAuthorization{}, err
	}

	// Check for existing active authorization (same gateway instance, host, repo, credential).
	existing, err := scanOfflineAuth(tx.QueryRow(ctx,
		"select "+offlineAuthColumns+" from offline_authorizations where gateway_instance_id=$1 and revoked_at is null and disabled_at is null for update",
		request.GatewayInstanceID))
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return domain.OfflineAuthorization{}, err
	}
	hasExisting := err == nil

	if hasExisting {
		// Exact replay: same owner, agent, delivery, hash. Return existing.
		if existing.Owner == request.Owner && existing.AgentID == request.AgentID &&
			existing.DeliveryID == request.DeliveryID && existing.ConfigurationHash == request.ConfigurationHash &&
			existing.HostID == r.HostID && existing.RepositoryID == r.ID &&
			existing.GatewayID == r.GatewayID && existing.StorageCredentialID == r.StorageCredentialID {
			var now time.Time
			if err = tx.QueryRow(ctx, "select clock_timestamp()").Scan(&now); err != nil {
				return domain.OfflineAuthorization{}, err
			}
			if now.Before(existing.ExpiresAt) {
				return existing, tx.Commit(ctx)
			}
			return deny(domain.ErrAuthorizationExpired, r.ID)
		}
		// Conflict: different binding on same instance. Reject.
		return deny(domain.ErrOfflineAuthorization, r.ID)
	}

	// Also check per-host/repo/credential uniqueness (the partial unique indexes enforce this,
	// but checking explicitly gives a better error).
	var conflictExists bool
	err = tx.QueryRow(ctx,
		`select exists(select 1 from offline_authorizations
		 where (host_id=$1 or repository_id=$2 or storage_credential_id=$3)
		 and revoked_at is null and disabled_at is null)`,
		r.HostID, r.ID, r.StorageCredentialID).Scan(&conflictExists)
	if err != nil {
		return domain.OfflineAuthorization{}, err
	}
	if conflictExists {
		return deny(domain.ErrOfflineAuthorization, r.ID)
	}

	var now time.Time
	if err = tx.QueryRow(ctx, "select clock_timestamp()").Scan(&now); err != nil {
		return domain.OfflineAuthorization{}, err
	}

	a := domain.OfflineAuthorization{
		ID:                    request.ID,
		Owner:                 request.Owner,
		GatewayInstanceID:     request.GatewayInstanceID,
		AgentID:               request.AgentID,
		HostID:                r.HostID,
		RepositoryID:          r.ID,
		GatewayID:             r.GatewayID,
		StorageCredentialID:   r.StorageCredentialID,
		DeliveryID:            request.DeliveryID,
		ConfigurationHash:     request.ConfigurationHash,
		AuthorizedAt:          now,
		ExpiresAt:             now.Add(request.Lifetime),
		AuthorizationSequence: 1,
		CreatedAt:             now,
	}

	_, err = tx.Exec(ctx, `insert into offline_authorizations(
		id,owner,gateway_instance_id,agent_id,host_id,repository_id,gateway_id,
		storage_credential_id,delivery_id,configuration_hash,authorized_at,expires_at,
		authorization_sequence,renewed_at,revoked_at,disabled_at,created_at
	) values($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,null,null,null,$14)`,
		a.ID, a.Owner, a.GatewayInstanceID, a.AgentID, a.HostID, a.RepositoryID, a.GatewayID,
		a.StorageCredentialID, a.DeliveryID, a.ConfigurationHash, a.AuthorizedAt, a.ExpiresAt,
		a.AuthorizationSequence, a.CreatedAt)
	if err != nil {
		return domain.OfflineAuthorization{}, err
	}

	if err = offlineAuthAudit(ctx, tx, a.ID, a.RepositoryID, "OFFLINE_AUTH_ISSUED", "GRANTED", domain.AuditSuccess); err != nil {
		return domain.OfflineAuthorization{}, err
	}
	if err = offlineAuthOutbox(ctx, tx, a, "OFFLINE_AUTH_ISSUED"); err != nil {
		return domain.OfflineAuthorization{}, err
	}

	// Verify the authorization is still valid after audit chain contention.
	var valid bool
	if err = tx.QueryRow(ctx,
		"select exists(select 1 from offline_authorizations where id=$1 and revoked_at is null and disabled_at is null and expires_at>clock_timestamp())",
		a.ID).Scan(&valid); err != nil {
		return domain.OfflineAuthorization{}, err
	}
	if !valid {
		return domain.OfflineAuthorization{}, domain.ErrOfflineAuthorization
	}

	return a, tx.Commit(ctx)
}

// RenewOfflineAuthorization extends an existing authorization's expiry and
// increments the sequence for anti-rollback. The current sequence MUST match
// exactly; the new expiry MUST NOT exceed 12h from the original authorization.
// Revoked/disabled/expired grants cannot be renewed.
func (s *Store) RenewOfflineAuthorization(ctx context.Context, request domain.OfflineRenewalRequest) (domain.OfflineAuthorization, error) {
	if err := request.Validate(12 * time.Hour); err != nil {
		return domain.OfflineAuthorization{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return domain.OfflineAuthorization{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	a, err := scanOfflineAuth(tx.QueryRow(ctx,
		"select "+offlineAuthColumns+" from offline_authorizations where id=$1 and owner=$2 for update",
		request.AuthorizationID, request.Owner))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.OfflineAuthorization{}, rejectOfflineAuth(ctx, tx, request.AuthorizationID, uuid.Nil, domain.ErrOfflineAuthorization)
	}
	if err != nil {
		return domain.OfflineAuthorization{}, err
	}

	// Anti-rollback: sequence must match exactly.
	if a.AuthorizationSequence != request.CurrentSequence {
		return domain.OfflineAuthorization{}, rejectOfflineAuth(ctx, tx, a.ID, a.RepositoryID, domain.ErrAntiRollback)
	}

	// Must be active (not revoked, disabled, or expired).
	var now time.Time
	if err = tx.QueryRow(ctx, "select clock_timestamp()").Scan(&now); err != nil {
		return domain.OfflineAuthorization{}, err
	}
	if a.RevokedAt != nil || a.DisabledAt != nil || !now.Before(a.ExpiresAt) {
		return domain.OfflineAuthorization{}, rejectOfflineAuth(ctx, tx, a.ID, a.RepositoryID, domain.ErrAuthorizationExpired)
	}

	// New expiry must not exceed 12h from original authorization.
	if request.NewExpiresAt.After(a.AuthorizedAt.Add(12 * time.Hour)) {
		return domain.OfflineAuthorization{}, rejectOfflineAuth(ctx, tx, a.ID, a.RepositoryID, domain.ErrOfflineAuthorization)
	}
	if !request.NewExpiresAt.After(now) {
		return domain.OfflineAuthorization{}, rejectOfflineAuth(ctx, tx, a.ID, a.RepositoryID, domain.ErrOfflineAuthorization)
	}

	// Configuration hash must match.
	if a.ConfigurationHash != request.ConfigurationHash {
		return domain.OfflineAuthorization{}, rejectOfflineAuth(ctx, tx, a.ID, a.RepositoryID, domain.ErrOfflineAuthorization)
	}

	// Re-verify the underlying bindings are still valid.
	r, err := lockAgentRepository(ctx, tx, a.AgentID, true)
	if errors.Is(err, domain.ErrNotFound) || r.Status == "LOCKED" || r.Status == "DISABLED" {
		return domain.OfflineAuthorization{}, rejectOfflineAuth(ctx, tx, a.ID, a.RepositoryID, domain.ErrOfflineAuthorization)
	}
	if err != nil {
		return domain.OfflineAuthorization{}, err
	}
	d, err := scanAgentDelivery(ctx, tx, a.AgentID)
	if errors.Is(err, pgx.ErrNoRows) || d.AcceptedAt == nil || d.ConfigurationHash != request.ConfigurationHash {
		return domain.OfflineAuthorization{}, rejectOfflineAuth(ctx, tx, a.ID, a.RepositoryID, domain.ErrOfflineAuthorization)
	}
	if err != nil {
		return domain.OfflineAuthorization{}, err
	}

	newSeq := a.AuthorizationSequence + 1
	_, err = tx.Exec(ctx,
		`update offline_authorizations set expires_at=$1, authorization_sequence=$2, renewed_at=$3
		 where id=$4 and owner=$5 and authorization_sequence=$6`,
		request.NewExpiresAt, newSeq, now, a.ID, a.Owner, request.CurrentSequence)
	if err != nil {
		return domain.OfflineAuthorization{}, err
	}

	a.ExpiresAt = request.NewExpiresAt
	a.AuthorizationSequence = newSeq
	a.RenewedAt = &now

	if err = offlineAuthAudit(ctx, tx, a.ID, a.RepositoryID, "OFFLINE_AUTH_RENEWED", "RENEWED", domain.AuditSuccess); err != nil {
		return domain.OfflineAuthorization{}, err
	}
	if err = offlineAuthOutbox(ctx, tx, a, "OFFLINE_AUTH_RENEWED"); err != nil {
		return domain.OfflineAuthorization{}, err
	}

	return a, tx.Commit(ctx)
}

// RevokeOfflineAuthorization marks an authorization as revoked. This is a
// central-only administrative action; the Gateway detects revocation on its
// next check or when the authorization expires.
func (s *Store) RevokeOfflineAuthorization(ctx context.Context, id, owner uuid.UUID) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	a, err := scanOfflineAuth(tx.QueryRow(ctx,
		"select "+offlineAuthColumns+" from offline_authorizations where id=$1 and owner=$2 for update", id, owner))
	if errors.Is(err, pgx.ErrNoRows) {
		return rejectOfflineAuth(ctx, tx, id, uuid.Nil, domain.ErrOfflineAuthorization)
	}
	if err != nil {
		return err
	}
	if a.RevokedAt != nil {
		return tx.Commit(ctx) // Idempotent.
	}

	var now time.Time
	if err = tx.QueryRow(ctx, "select clock_timestamp()").Scan(&now); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, "update offline_authorizations set revoked_at=$1 where id=$2 and owner=$3", now, id, owner); err != nil {
		return err
	}

	if err = offlineAuthAudit(ctx, tx, id, a.RepositoryID, "OFFLINE_AUTH_REVOKED", "REVOKED", domain.AuditSuccess); err != nil {
		return err
	}
	if err = offlineAuthOutbox(ctx, tx, a, "OFFLINE_AUTH_REVOKED"); err != nil {
		return err
	}

	return tx.Commit(ctx)
}

// DisableOfflineAuthorization marks an authorization as disabled (e.g., when
// the credential is disabled or the Agent is revoked).
func (s *Store) DisableOfflineAuthorization(ctx context.Context, id, owner uuid.UUID) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	a, err := scanOfflineAuth(tx.QueryRow(ctx,
		"select "+offlineAuthColumns+" from offline_authorizations where id=$1 and owner=$2 for update", id, owner))
	if errors.Is(err, pgx.ErrNoRows) {
		return rejectOfflineAuth(ctx, tx, id, uuid.Nil, domain.ErrOfflineAuthorization)
	}
	if err != nil {
		return err
	}
	if a.DisabledAt != nil {
		return tx.Commit(ctx)
	}

	var now time.Time
	if err = tx.QueryRow(ctx, "select clock_timestamp()").Scan(&now); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, "update offline_authorizations set disabled_at=$1 where id=$2 and owner=$3", now, id, owner); err != nil {
		return err
	}

	if err = offlineAuthAudit(ctx, tx, id, a.RepositoryID, "OFFLINE_AUTH_DISABLED", "DISABLED", domain.AuditSuccess); err != nil {
		return err
	}

	return tx.Commit(ctx)
}

// VerifyOfflineAuthorization checks that the authorization is still valid:
// not revoked, not disabled, not expired, bindings match, and sequence is at
// least the provided minimum. Used by the Gateway to verify on each use.
func (s *Store) VerifyOfflineAuthorization(ctx context.Context, id, owner uuid.UUID, minSequence int64) (domain.OfflineAuthorization, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return domain.OfflineAuthorization{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	a, err := scanOfflineAuth(tx.QueryRow(ctx,
		"select "+offlineAuthColumns+" from offline_authorizations where id=$1 and owner=$2", id, owner))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.OfflineAuthorization{}, domain.ErrOfflineAuthorization
	}
	if err != nil {
		return domain.OfflineAuthorization{}, err
	}

	if a.RevokedAt != nil || a.DisabledAt != nil {
		return domain.OfflineAuthorization{}, domain.ErrAuthorizationRevoked
	}
	var now time.Time
	if err = tx.QueryRow(ctx, "select clock_timestamp()").Scan(&now); err != nil {
		return domain.OfflineAuthorization{}, err
	}
	if !now.Before(a.ExpiresAt) {
		return domain.OfflineAuthorization{}, domain.ErrAuthorizationExpired
	}
	if a.AuthorizationSequence < minSequence {
		return domain.OfflineAuthorization{}, domain.ErrAntiRollback
	}

	// Re-verify bindings.
	r, err := lockAgentRepository(ctx, tx, a.AgentID, true)
	if errors.Is(err, domain.ErrNotFound) || r.Status == "LOCKED" || r.Status == "DISABLED" {
		return domain.OfflineAuthorization{}, domain.ErrOfflineAuthorization
	}
	if err != nil {
		return domain.OfflineAuthorization{}, err
	}
	d, err := scanAgentDelivery(ctx, tx, a.AgentID)
	if errors.Is(err, pgx.ErrNoRows) || d.AcceptedAt == nil || d.ConfigurationHash != a.ConfigurationHash {
		return domain.OfflineAuthorization{}, domain.ErrOfflineAuthorization
	}
	if err != nil {
		return domain.OfflineAuthorization{}, err
	}

	return a, tx.Commit(ctx)
}

// OfflineAuthorizationForGateway retrieves the current authorization for a
// specific gateway instance, if any active one exists. Used during Gateway
// startup to recover state.
func (s *Store) OfflineAuthorizationForGateway(ctx context.Context, gatewayInstanceID uuid.UUID) (domain.OfflineAuthorization, error) {
	var a domain.OfflineAuthorization
	err := s.pool.QueryRow(ctx,
		"select "+offlineAuthColumns+" from offline_authorizations where gateway_instance_id=$1 and revoked_at is null and disabled_at is null and expires_at>clock_timestamp()",
		gatewayInstanceID).Scan(&a.ID, &a.Owner, &a.GatewayInstanceID, &a.AgentID, &a.HostID, &a.RepositoryID,
		&a.GatewayID, &a.StorageCredentialID, &a.DeliveryID, &a.ConfigurationHash,
		&a.AuthorizedAt, &a.ExpiresAt, &a.AuthorizationSequence,
		&a.RenewedAt, &a.RevokedAt, &a.DisabledAt, &a.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.OfflineAuthorization{}, domain.ErrOfflineAuthorization
	}
	return a, err
}

func offlineAuthAudit(ctx context.Context, tx pgx.Tx, id, repositoryID uuid.UUID, action, reason string, result string) error {
	auditID, err := uuid.NewV7()
	if err != nil {
		return err
	}
	var now time.Time
	if err = tx.QueryRow(ctx, "select clock_timestamp()").Scan(&now); err != nil {
		return err
	}
	return appendAudit(ctx, tx, domain.AuditEvent{
		ID: auditID, OccurredAt: now, ActorType: domain.ActorSystem,
		Action: action, ResourceType: "REPOSITORY", ResourceID: repositoryID,
		RequestID: id, Result: result, ReasonCode: reason,
	})
}

func offlineAuthOutbox(ctx context.Context, tx pgx.Tx, a domain.OfflineAuthorization, event string) error {
	id, err := uuid.NewV7()
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx,
		`insert into outbox_events(id,event_type,aggregate_type,aggregate_id,payload,created_at,available_at)
		 values($1,$2,'OFFLINE_AUTHORIZATION',$3,'{}',clock_timestamp(),clock_timestamp())`,
		id, event, a.ID)
	return err
}

func rejectOfflineAuth(ctx context.Context, tx pgx.Tx, id, repositoryID uuid.UUID, cause error) error {
	if err := offlineAuthAudit(ctx, tx, id, repositoryID, "OFFLINE_AUTH_DENIED", "REJECTED", domain.AuditDenied); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	return cause
}
