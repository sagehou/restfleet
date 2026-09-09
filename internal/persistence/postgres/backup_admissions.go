package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/sagehou/restfleet/internal/domain"
)

const admissionColumns = "id,owner,agent_id,host_id,repository_id,gateway_id,storage_credential_id,delivery_id,gateway_secret_ref,restic_secret_ref,configuration_hash,created_at,expires_at,released_at"

func scanBackupAdmission(row rowScanner) (domain.BackupAdmission, error) {
	var a domain.BackupAdmission
	err := row.Scan(&a.ID, &a.Owner, &a.AgentID, &a.HostID, &a.RepositoryID, &a.GatewayID, &a.StorageCredentialID,
		&a.DeliveryID, &a.GatewaySecretRef, &a.ResticSecretRef, &a.ConfigurationHash, &a.CreatedAt, &a.ExpiresAt, &a.ReleasedAt)
	return a, err
}

// Caller MUST hold the credential row lock. This is shared by enqueue, claim,
// refresh/completion and replacement, not only the new admission entrypoint.
func ensureNoBackupAdmission(ctx context.Context, tx pgx.Tx, credentialID uuid.UUID) error {
	var busy bool
	err := tx.QueryRow(ctx, "select exists(select 1 from gateway_backup_admissions where storage_credential_id=$1 and released_at is null)", credentialID).Scan(&busy)
	if err != nil {
		return err
	}
	if busy {
		return domain.ErrRepositoryBusy
	}
	return nil
}

func backupAdmissionAudit(ctx context.Context, tx pgx.Tx, id, repositoryID uuid.UUID, action, reason string, result string) error {
	auditID, err := uuid.NewV7()
	if err != nil {
		return err
	}
	var now time.Time
	if err = tx.QueryRow(ctx, "select clock_timestamp()").Scan(&now); err != nil {
		return err
	}
	return appendAudit(ctx, tx, domain.AuditEvent{ID: auditID, OccurredAt: now, ActorType: domain.ActorSystem,
		Action: action, ResourceType: "REPOSITORY", ResourceID: repositoryID, RequestID: id, Result: result, ReasonCode: reason})
}

func rejectBackupAdmission(ctx context.Context, tx pgx.Tx, id, repositoryID uuid.UUID, cause error) error {
	if err := backupAdmissionAudit(ctx, tx, id, repositoryID, "GATEWAY_BACKUP_ADMISSION_DENIED", "REJECTED", domain.AuditDenied); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	return cause
}

func admissionOutbox(ctx context.Context, tx pgx.Tx, a domain.BackupAdmission, event string) error {
	id, err := uuid.NewV7()
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `insert into outbox_events(id,event_type,aggregate_type,aggregate_id,payload,created_at,available_at)
		values($1,$2,'GATEWAY_ADMISSION',$3,'{}',clock_timestamp(),clock_timestamp())`, id, event, a.ID)
	return err
}

func admissionMatches(a domain.BackupAdmission, r domain.Repository, d domain.AgentCredentialDelivery, hash string) bool {
	return a.HostID == r.HostID && a.RepositoryID == r.ID && a.GatewayID == r.GatewayID && a.StorageCredentialID == r.StorageCredentialID &&
		a.DeliveryID == d.ID && d.AcceptedAt != nil && d.Repository.ID == r.ID && d.ConfigurationHash == hash && a.ConfigurationHash == hash &&
		a.GatewaySecretRef == r.GatewaySecretRef && a.ResticSecretRef == r.ResticSecretRef && d.Gateway.ID == r.GatewaySecretRef && d.Restic.ID == r.ResticSecretRef
}

// ReserveBackupAdmission reserves exclusive data-plane ownership without
// decrypting secrets, dispatching a backup or publishing a public route. The
// trusted caller MUST persist its owner identity and reconcile cleanup after
// crashes; it cannot silently take over an expired but unreleased reservation.
func (s *Store) ReserveBackupAdmission(ctx context.Context, request domain.BackupAdmissionRequest) (domain.BackupAdmission, error) {
	if err := request.Validate(); err != nil {
		return domain.BackupAdmission{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return domain.BackupAdmission{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	deny := func(cause error, repoID uuid.UUID) (domain.BackupAdmission, error) {
		return domain.BackupAdmission{}, rejectBackupAdmission(ctx, tx, request.ID, repoID, cause)
	}
	// Agent -> Host -> Repository -> exclusive credential -> admission -> audit.
	r, err := lockAgentRepository(ctx, tx, request.AgentID, true)
	if errors.Is(err, domain.ErrNotFound) {
		return deny(domain.ErrBackupAdmission, uuid.Nil)
	}
	if err != nil {
		return domain.BackupAdmission{}, err
	}
	if r.Status == "LOCKED" {
		return deny(domain.ErrBackupAdmission, r.ID)
	}
	d, err := scanAgentDelivery(ctx, tx, request.AgentID)
	if errors.Is(err, pgx.ErrNoRows) {
		return deny(domain.ErrBackupAdmission, r.ID)
	}
	if err != nil {
		return domain.BackupAdmission{}, err
	}
	if _, err = tx.Exec(ctx, "select pg_advisory_xact_lock(hashtextextended($1,0))", r.ID.String()); err != nil {
		return domain.BackupAdmission{}, err
	}
	a, err := scanBackupAdmission(tx.QueryRow(ctx, "select "+admissionColumns+" from gateway_backup_admissions where id=$1 for update", request.ID))
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return a, err
	}
	existing := err == nil
	if !existing {
		a = domain.BackupAdmission{ID: request.ID, Owner: request.Owner, AgentID: request.AgentID, HostID: r.HostID, RepositoryID: r.ID,
			GatewayID: r.GatewayID, StorageCredentialID: r.StorageCredentialID, DeliveryID: request.DeliveryID,
			GatewaySecretRef: r.GatewaySecretRef, ResticSecretRef: r.ResticSecretRef, ConfigurationHash: request.ConfigurationHash}
	}
	if a.Owner != request.Owner || a.AgentID != request.AgentID || a.DeliveryID != request.DeliveryID ||
		!admissionMatches(a, r, d, request.ConfigurationHash) || a.ReleasedAt != nil ||
		(existing && a.ExpiresAt.Sub(a.CreatedAt) != request.Lifetime) {
		return deny(domain.ErrBackupAdmission, r.ID)
	}
	var now time.Time
	if err = tx.QueryRow(ctx, "select clock_timestamp()").Scan(&now); err != nil {
		return a, err
	}
	if existing {
		if !now.Before(a.ExpiresAt) {
			return deny(domain.ErrBackupAdmission, r.ID)
		}
		return a, tx.Commit(ctx) // Exact replay cannot extend the deadline or reissue work.
	}
	if err = ensureNoBackupAdmission(ctx, tx, r.StorageCredentialID); errors.Is(err, domain.ErrRepositoryBusy) {
		return deny(err, r.ID)
	}
	if err != nil {
		return a, err
	}
	var busy bool
	err = tx.QueryRow(ctx, `select exists(select 1 from operations where storage_credential_id=$1 and finished_at is null)
		or exists(select 1 from repository_leases l join repositories r on r.id=l.repository_id
		where r.storage_credential_id=$1 and l.expires_at>clock_timestamp())`, r.StorageCredentialID).Scan(&busy)
	if err != nil {
		return a, err
	}
	if busy {
		return deny(domain.ErrRepositoryBusy, r.ID)
	}
	a.CreatedAt, a.ExpiresAt = now, now.Add(request.Lifetime)
	_, err = tx.Exec(ctx, `insert into gateway_backup_admissions(`+admissionColumns+`) values($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,null)`,
		a.ID, a.Owner, a.AgentID, a.HostID, a.RepositoryID, a.GatewayID, a.StorageCredentialID, a.DeliveryID, a.GatewaySecretRef, a.ResticSecretRef, a.ConfigurationHash, a.CreatedAt, a.ExpiresAt)
	if err != nil {
		return a, err
	}
	if err = admissionOutbox(ctx, tx, a, "GATEWAY_BACKUP_ADMITTED"); err != nil {
		return a, err
	}
	if err = backupAdmissionAudit(ctx, tx, a.ID, a.RepositoryID, "GATEWAY_BACKUP_ADMISSION", "RESERVED", domain.AuditSuccess); err != nil {
		return a, err
	}
	// Audit chain contention cannot consume the validity window unnoticed.
	var valid bool
	if err = tx.QueryRow(ctx, "select exists(select 1 from gateway_backup_admissions where id=$1 and released_at is null and expires_at>clock_timestamp())", a.ID).Scan(&valid); err != nil {
		return a, err
	}
	if !valid {
		return domain.BackupAdmission{}, domain.ErrBackupAdmission
	}
	return a, tx.Commit(ctx)
}

// CheckBackupAdmission rechecks current bindings/ACK/status before online use.
// It does not release or extend the reservation, read secrets or authorize DB-
// offline use. A future offline grant must have its own durable validation.
func (s *Store) CheckBackupAdmission(ctx context.Context, id, owner uuid.UUID, configurationHash string) (domain.BackupAdmission, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return domain.BackupAdmission{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	a, err := lockBackupAdmission(ctx, tx, id, owner, configurationHash)
	if errors.Is(err, domain.ErrBackupAdmission) {
		return domain.BackupAdmission{}, rejectBackupAdmission(ctx, tx, id, uuid.Nil, err)
	}
	if err != nil {
		return domain.BackupAdmission{}, err
	}
	return a, tx.Commit(ctx)
}

// Caller commits or rolls back; no secret reads/writes precede these locks.
func lockBackupAdmission(ctx context.Context, tx pgx.Tx, id, owner uuid.UUID, configurationHash string) (domain.BackupAdmission, error) {
	a, err := scanBackupAdmission(tx.QueryRow(ctx, "select "+admissionColumns+" from gateway_backup_admissions where id=$1 and owner=$2", id, owner))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.BackupAdmission{}, domain.ErrBackupAdmission
	}
	if err != nil {
		return domain.BackupAdmission{}, err
	}
	r, err := lockAgentRepository(ctx, tx, a.AgentID, true)
	if errors.Is(err, domain.ErrNotFound) {
		return domain.BackupAdmission{}, domain.ErrBackupAdmission
	}
	if err != nil {
		return domain.BackupAdmission{}, err
	}
	d, err := scanAgentDelivery(ctx, tx, a.AgentID)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.BackupAdmission{}, domain.ErrBackupAdmission
	}
	if err != nil {
		return domain.BackupAdmission{}, err
	}
	a, err = scanBackupAdmission(tx.QueryRow(ctx, "select "+admissionColumns+" from gateway_backup_admissions where id=$1 and owner=$2 and released_at is null and expires_at>clock_timestamp() for update", id, owner))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.BackupAdmission{}, domain.ErrBackupAdmission
	}
	if err != nil {
		return domain.BackupAdmission{}, err
	}
	if r.Status == "LOCKED" || !admissionMatches(a, r, d, configurationHash) {
		return domain.BackupAdmission{}, domain.ErrBackupAdmission
	}
	return a, nil
}

// ReleaseBackupAdmission is CENTRAL-ONLY cleanup acknowledgement, never an
// Agent endpoint or expiry reaper. The owner MUST first revoke/join all public
// requests, sessions, subprocesses and refresh persistence. This call does not
// itself prove cleanup; interrupted/uncertain cleanup MUST keep the fence.
func (s *Store) ReleaseBackupAdmission(ctx context.Context, id, owner uuid.UUID) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	a, err := scanBackupAdmission(tx.QueryRow(ctx, "select "+admissionColumns+" from gateway_backup_admissions where id=$1 and owner=$2 for update", id, owner))
	if errors.Is(err, pgx.ErrNoRows) {
		return rejectBackupAdmission(ctx, tx, id, uuid.Nil, domain.ErrBackupAdmission)
	}
	if err != nil {
		return err
	}
	if a.ReleasedAt != nil {
		return tx.Commit(ctx)
	}
	if _, err = tx.Exec(ctx, "update gateway_backup_admissions set released_at=clock_timestamp() where id=$1 and owner=$2", id, owner); err != nil {
		return err
	}
	if err = admissionOutbox(ctx, tx, a, "GATEWAY_BACKUP_RELEASED"); err != nil {
		return err
	}
	if err = backupAdmissionAudit(ctx, tx, id, a.RepositoryID, "GATEWAY_BACKUP_RELEASE", "CLEANUP_CONFIRMED", domain.AuditSuccess); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
