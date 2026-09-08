package postgres

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/sagehou/restfleet/internal/domain"
)

const repositoryColumns = "id,host_id,storage_credential_id,name,status,backend_path,gateway_username,gateway_secret_ref,restic_secret_ref,gateway_secret_revision,restic_secret_revision,revision,format_version,created_at,updated_at,coalesce(restic_id,''),initialized_at,last_initialize_operation_id" +
	",(select d.revision from repository_agent_deliveries d join agents a on a.id=d.agent_id and a.status='ACTIVE' where d.repository_id=repositories.id and a.host_id=repositories.host_id and d.gateway_secret_ref=repositories.gateway_secret_ref and d.restic_secret_ref=repositories.restic_secret_ref)" +
	",(select d.accepted_at from repository_agent_deliveries d join agents a on a.id=d.agent_id and a.status='ACTIVE' where d.repository_id=repositories.id and a.host_id=repositories.host_id and d.gateway_secret_ref=repositories.gateway_secret_ref and d.restic_secret_ref=repositories.restic_secret_ref)"

func scanRepository(row rowScanner) (domain.Repository, error) {
	var r domain.Repository
	err := row.Scan(&r.ID, &r.HostID, &r.StorageCredentialID, &r.Name, &r.Status, &r.BackendPath,
		&r.GatewayID, &r.GatewaySecretRef, &r.ResticSecretRef, &r.GatewaySecretRevision, &r.ResticSecretRevision,
		&r.Revision, &r.FormatVersion, &r.CreatedAt, &r.UpdatedAt, &r.ResticID, &r.InitializedAt, &r.LastInitializeOperationID, &r.AgentCredentialRevision, &r.AgentCredentialAcceptedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, domain.ErrNotFound
	}
	return r, err
}

func (s *Store) Repository(ctx context.Context, id uuid.UUID) (domain.Repository, error) {
	return scanRepository(s.pool.QueryRow(ctx, "select "+repositoryColumns+" from repositories where id=$1", id))
}

func (s *Store) Repositories(ctx context.Context, after uuid.UUID, limit int) ([]domain.Repository, error) {
	rows, err := s.pool.Query(ctx, "select "+repositoryColumns+" from repositories where archived_at is null and id>$1 order by id limit $2", after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]domain.Repository, 0)
	for rows.Next() {
		r, err := scanRepository(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, r)
	}
	return items, rows.Err()
}

func (s *Store) RepositoryCount(ctx context.Context) (int64, error) {
	var count int64
	err := s.pool.QueryRow(ctx, "select count(*) from repositories where archived_at is null").Scan(&count)
	return count, err
}

// Host -> credential -> audit is the lock order. FK/status/ownership validation,
// independent encrypted versions and the audit all commit or roll back together.
func (s *Store) CreateRepository(ctx context.Context, r domain.Repository, gateway, restic domain.SecretEnvelope, audit domain.AuditEvent) (domain.Repository, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return domain.Repository{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var status string
	err = tx.QueryRow(ctx, "select status from hosts where id=$1 for update", r.HostID).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Repository{}, domain.ErrNotFound
	}
	if err != nil {
		return domain.Repository{}, err
	}
	if status != "PENDING" && status != "ACTIVE" {
		return domain.Repository{}, domain.ErrHostUnavailable
	}
	var exists bool
	if err = tx.QueryRow(ctx, "select exists(select 1 from repositories where host_id=$1 and archived_at is null)", r.HostID).Scan(&exists); err != nil {
		return domain.Repository{}, err
	}
	if exists {
		return domain.Repository{}, domain.ErrHostRepositoryExists
	}
	err = tx.QueryRow(ctx, "select status from storage_credentials where id=$1 for update", r.StorageCredentialID).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Repository{}, domain.ErrNotFound
	}
	if err != nil {
		return domain.Repository{}, err
	}
	if status == "DISABLED" {
		return domain.Repository{}, domain.ErrCredentialDisabled
	}
	if err = insertSecret(ctx, tx, gateway); err != nil {
		return domain.Repository{}, err
	}
	if err = insertSecret(ctx, tx, restic); err != nil {
		return domain.Repository{}, err
	}
	_, err = tx.Exec(ctx, `insert into repositories(id,host_id,storage_credential_id,name,status,backend_path,gateway_username,
		gateway_secret_ref,restic_secret_ref,gateway_secret_revision,restic_secret_revision,revision,created_at,updated_at)
		values($1,$2,$3,$4,'PROVISIONING',$5,$6,$7,$8,1,1,1,$9,$9)`,
		r.ID, r.HostID, r.StorageCredentialID, r.Name, r.BackendPath, r.GatewayID, gateway.ID, restic.ID, r.CreatedAt)
	if err != nil {
		return domain.Repository{}, persistenceError(err)
	}
	for _, revision := range []struct {
		kind   string
		secret uuid.UUID
	}{{"GATEWAY", gateway.ID}, {"RESTIC_KEY", restic.ID}} {
		id, err := uuid.NewV7()
		if err != nil {
			return domain.Repository{}, err
		}
		_, err = tx.Exec(ctx, `insert into repository_credential_revisions(id,repository_id,kind,revision,secret_ref,valid_from,created_at)
			values($1,$2,$3,1,$4,$5,$5)`, id, r.ID, revision.kind, revision.secret, r.CreatedAt)
		if err != nil {
			return domain.Repository{}, err
		}
	}
	if err = appendAudit(ctx, tx, audit); err != nil {
		return domain.Repository{}, err
	}
	return r, tx.Commit(ctx)
}

// RepositoryResticSecret is a central worker-only read, after the job's secret
// access audit and leases commit. It cannot fetch gateway or provider secrets.
func (s *Store) RepositoryResticSecret(ctx context.Context, id uuid.UUID) (domain.SecretEnvelope, error) {
	var e domain.SecretEnvelope
	err := s.pool.QueryRow(ctx, `select s.id,s.kind,s.algorithm,s.key_id,s.ciphertext,s.nonce,s.wrapped_data_key,s.wrap_nonce,s.aad,s.created_at
		from repositories r join repository_credential_revisions v on v.repository_id=r.id and v.kind='RESTIC_KEY'
		and v.revision=r.restic_secret_revision and v.secret_ref=r.restic_secret_ref
		join secrets s on s.id=v.secret_ref where r.id=$1`, id).
		Scan(&e.ID, &e.Kind, &e.Algorithm, &e.KeyID, &e.Ciphertext, &e.Nonce, &e.WrappedDataKey, &e.WrapNonce, &e.AAD, &e.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return e, domain.ErrNotFound
	}
	return e, err
}
