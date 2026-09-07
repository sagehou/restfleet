package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/sagehou/restfleet/internal/domain"
)

// Host -> repository -> credential -> lease -> audit follows the existing
// creation lock order. No repository identity may change during provisioning.
func lockProvisionRepository(ctx context.Context, tx pgx.Tx, id uuid.UUID) (domain.Repository, error) {
	var hostID uuid.UUID
	err := tx.QueryRow(ctx, "select host_id from repositories where id=$1", id).Scan(&hostID)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Repository{}, domain.ErrNotFound
	}
	if err != nil {
		return domain.Repository{}, err
	}
	var status string
	if err = tx.QueryRow(ctx, "select status from hosts where id=$1 for update", hostID).Scan(&status); err != nil {
		return domain.Repository{}, err
	}
	r, err := scanRepository(tx.QueryRow(ctx, "select "+repositoryColumns+" from repositories where id=$1 for update", id))
	if err != nil {
		return r, err
	}
	if status != "PENDING" && status != "ACTIVE" {
		return r, domain.ErrHostUnavailable
	}
	if r.Status != "PROVISIONING" || r.InitializedAt != nil {
		return r, domain.ErrRepositoryUnavailable
	}
	return r, nil
}

func acquireProvisionLease(ctx context.Context, tx pgx.Tx, job domain.CredentialJob, now time.Time) error {
	r := job.Repository
	// The repository advisory lock coordinates future backup/maintenance lease
	// admission. The row remains authoritative after the transaction ends.
	if _, err := tx.Exec(ctx, "select pg_advisory_xact_lock(hashtextextended($1,0))", r.ID.String()); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `insert into repository_leases(repository_id,operation_id,owner,kind,expires_at)
		values($1,$2,$3,'MAINTENANCE',$4) on conflict(repository_id) do update
		set operation_id=excluded.operation_id,owner=excluded.owner,kind=excluded.kind,expires_at=excluded.expires_at
		where repository_leases.expires_at<=clock_timestamp()`, r.ID, job.Operation.ID, job.Owner, now.Add(30*time.Second))
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return domain.ErrRepositoryBusy
	}
	return nil
}

func ensureProvisionLease(ctx context.Context, tx pgx.Tx, job domain.CredentialJob) error {
	if job.Repository == nil {
		return nil
	}
	var valid bool
	err := tx.QueryRow(ctx, `select exists(select 1 from repository_leases where repository_id=$1 and operation_id=$2
		and owner=$3 and kind='MAINTENANCE' and expires_at>clock_timestamp())`, job.Repository.ID, job.Operation.ID, job.Owner).Scan(&valid)
	if err != nil {
		return err
	}
	if !valid {
		return domain.ErrJobLeaseLost
	}
	return nil
}

func (s *Store) CompleteRepositoryJob(ctx context.Context, id, owner uuid.UUID, code, resticID string, version int) error {
	if !domain.ValidInitializeCode(code) || (code == "" && !domain.ValidInitializedRepository(resticID, version)) {
		return domain.ErrOperationTransition
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	job, err := lockCredentialJob(ctx, tx, id, owner)
	if err != nil {
		return err
	}
	if job.Repository == nil {
		return domain.ErrOperationTransition
	}
	var now time.Time
	if err = tx.QueryRow(ctx, "select clock_timestamp()").Scan(&now); err != nil {
		return err
	}
	job.Operation.ErrorCode = code
	if !credentialUnchanged(job) {
		job.Operation.ErrorCode = "CREDENTIAL_CHANGED"
	}
	if job.Credential.Status == "DISABLED" {
		job.Operation.ErrorCode = "CREDENTIAL_DISABLED"
	}
	if !job.RepositoryAvailable {
		job.Operation.ErrorCode = "REPOSITORY_UNAVAILABLE"
	}
	to := "FAILED"
	if job.Operation.ErrorCode == "" {
		to = "SUCCEEDED"
	}
	if job.Operation.ErrorCode == "INITIALIZE_TIMED_OUT" {
		to = "TIMED_OUT"
	}
	if err = finishRepositoryJob(ctx, tx, &job, to, now, resticID, version); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func finishRepositoryJob(ctx context.Context, tx pgx.Tx, job *domain.CredentialJob, to string, now time.Time, resticID string, version int) error {
	if err := transitionOperation(ctx, tx, &job.Operation, to, now); err != nil {
		return err
	}
	if to == "SUCCEEDED" {
		if !domain.ValidInitializedRepository(resticID, version) || !credentialUnchanged(*job) {
			return domain.ErrOperationTransition
		}
		tag, err := tx.Exec(ctx, `update repositories set restic_id=$2,format_version=$3,initialized_at=$4,updated_at=$4,revision=revision+1
			where id=$1 and status='PROVISIONING' and initialized_at is null`, job.Repository.ID, resticID, version, now)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return domain.ErrRepositoryUnavailable
		}
	}
	audit, err := credentialJobAudit(job.Operation, "REPOSITORY_INITIALIZE_RESULT", to, now)
	if err != nil {
		return err
	}
	audit.ResourceType, audit.ResourceID = "REPOSITORY", job.Repository.ID
	if to != "SUCCEEDED" {
		audit.Result = domain.AuditFailure
	}
	if err = appendAudit(ctx, tx, audit); err != nil {
		return err
	}
	if job.Owner != uuid.Nil {
		if err = ensureProvisionLease(ctx, tx, *job); err != nil {
			return err
		}
	}
	// Exhausted crash recovery may retire only its own old lease, never a backup.
	_, err = tx.Exec(ctx, `update repository_leases set expires_at=clock_timestamp()
		where repository_id=$1 and operation_id=$2 and kind='MAINTENANCE'`, job.Repository.ID, job.Operation.ID)
	if err != nil {
		return err
	}
	return finishJobRow(ctx, tx, job, to, now)
}
