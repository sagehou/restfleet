package postgres

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/sagehou/restfleet/internal/domain"
)

// GatewayRuntimeCleanupComplete reports the exact signed terminal record for
// one runtime. Ordinary replay tail, release or a recovered origin never
// substitutes for this proof.
func (s *Store) GatewayRuntimeCleanupComplete(ctx context.Context, id, runtime uuid.UUID) (bool, error) {
	if id.Version() != 7 || id.Variant() != uuid.RFC4122 || runtime.Version() != 7 || runtime.Variant() != uuid.RFC4122 {
		return false, domain.ErrGatewayPending
	}
	var complete bool
	err := s.pool.QueryRow(ctx, `select exists(
		select 1 from gateway_pending_records p
		join audit_events e on e.id=p.record_id
		where p.admission_id=$1 and p.runtime_id=$2 and p.kind='audit'
		and e.action='GATEWAY_RUNTIME_CLEANUP_COMPLETE')`, id, runtime).Scan(&complete)
	return complete, err
}

// RecoverGatewayAdmission is CENTRAL-ONLY controlled recovery for an expired
// admission that has no terminal cleanup record. The caller MUST have stopped
// the old runtime and fenced its host outside this transaction; this method
// records the operator decision and releases the fence, but never fabricates a
// cleanup record, deletes queues or reconstructs the failed owner.
func (s *Store) RecoverGatewayAdmission(ctx context.Context, id, owner, runtime uuid.UUID, source ed25519.PublicKey) error {
	if id.Version() != 7 || id.Variant() != uuid.RFC4122 || owner.Version() != 7 || owner.Variant() != uuid.RFC4122 ||
		runtime.Version() != 7 || runtime.Variant() != uuid.RFC4122 ||
		(len(source) != 0 && len(source) != ed25519.PublicKeySize) {
		return domain.ErrBackupAdmission
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	a, err := scanBackupAdmission(tx.QueryRow(ctx, "select "+admissionColumns+" from gateway_backup_admissions where id=$1 and owner=$2", id, owner))
	if errors.Is(err, pgx.ErrNoRows) {
		return rejectBackupAdmission(ctx, tx, id, uuid.Nil, domain.ErrBackupAdmission)
	}
	if err != nil {
		return err
	}
	// Match the credential -> admission lock order used by material/refresh.
	if _, err = tx.Exec(ctx, "select id from storage_credentials where id=$1 for update", a.StorageCredentialID); err != nil {
		return err
	}
	a, err = scanBackupAdmission(tx.QueryRow(ctx, "select "+admissionColumns+" from gateway_backup_admissions where id=$1 and owner=$2 for update", id, owner))
	if errors.Is(err, pgx.ErrNoRows) {
		return rejectBackupAdmission(ctx, tx, id, uuid.Nil, domain.ErrBackupAdmission)
	}
	if err != nil {
		return err
	}
	deny := func() error {
		return rejectBackupAdmission(ctx, tx, id, a.RepositoryID, domain.ErrBackupAdmission)
	}
	if a.ReleasedAt != nil {
		return tx.Commit(ctx)
	}
	var now time.Time
	if err = tx.QueryRow(ctx, "select clock_timestamp()").Scan(&now); err != nil {
		return err
	}
	// A live admission still has a usable data path. Controlled recovery only
	// applies after the original deadline, never as an early release shortcut.
	if now.Before(a.ExpiresAt) {
		return deny()
	}
	var terminal bool
	if err = tx.QueryRow(ctx, `select exists(
		select 1 from gateway_pending_records p
		join audit_events e on e.id=p.record_id
		where p.admission_id=$1 and p.runtime_id=$2 and p.kind='audit'
		and e.action='GATEWAY_RUNTIME_CLEANUP_COMPLETE')`, id, runtime).Scan(&terminal); err != nil {
		return err
	}
	// A terminal record has its own atomic sealing/release path. Reusing
	// recovery would mislabel a proven cleanup.
	if terminal {
		return deny()
	}
	var originRuntime uuid.UUID
	var originSource ed25519.PublicKey
	var closedAt *time.Time
	hasOrigin := false
	err = tx.QueryRow(ctx, `select runtime_id,public_key,closed_at from gateway_pending_origins
		where admission_id=$1 and runtime_id=$2 for update`, id, runtime).Scan(&originRuntime, &originSource, &closedAt)
	switch {
	case err == nil:
		if originRuntime != runtime || len(source) != ed25519.PublicKeySize || !bytes.Equal(originSource, source) {
			return deny()
		}
		hasOrigin = true
	case errors.Is(err, pgx.ErrNoRows):
		// No origin means initialization never completed. An operator-supplied
		// source would then identify a different runtime and must be rejected.
		if len(source) != 0 {
			return deny()
		}
	default:
		return err
	}
	requestID, err := uuid.NewV7()
	if err != nil {
		return err
	}
	if err = revokeGatewayRuntime(ctx, tx, a, runtime, requestID, now); err != nil {
		return err
	}
	if hasOrigin && closedAt == nil {
		if _, err = tx.Exec(ctx, "update gateway_pending_origins set closed_at=$3 where admission_id=$1 and runtime_id=$2", id, runtime, now); err != nil {
			return err
		}
		if err = backupAdmissionAudit(ctx, tx, requestID, a.RepositoryID, "GATEWAY_PENDING_ORIGIN", "RECOVERED", domain.AuditSuccess); err != nil {
			return err
		}
	}
	if err = ensureGatewayAuthorizationReleasable(ctx, tx, id); err != nil {
		return err
	}
	if err = ensureGatewayPendingReleasable(ctx, tx, id); err != nil {
		return err
	}
	if err = releaseBackupAdmission(ctx, tx, a, "GATEWAY_BACKUP_RECOVERY", "RECOVERY_CONFIRMED"); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func revokeGatewayRuntime(ctx context.Context, tx pgx.Tx, a domain.BackupAdmission, runtime uuid.UUID, requestID uuid.UUID, now time.Time) error {
	var revision int64
	var revoked bool
	var issued time.Time
	err := tx.QueryRow(ctx, `select revision,revoked,issued_at from gateway_authorization_decisions
		where admission_id=$1 and runtime_id=$2 order by revision desc limit 1`, a.ID, runtime).Scan(&revision, &revoked, &issued)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if now.Before(issued) {
		return domain.ErrGatewayPending
	}
	if revoked {
		return nil
	}
	truncated := now.Truncate(time.Second)
	if truncated.Before(issued) {
		return domain.ErrGatewayPending
	}
	if _, err = tx.Exec(ctx, `insert into gateway_authorization_decisions
		(admission_id,revision,request_id,runtime_id,lifetime_seconds,issued_at,expires_at,revoked)
		values($1,$2,$3,$4,0,$5,null,true)`, a.ID, revision+1, requestID, runtime, truncated); err != nil {
		return err
	}
	if err = admissionOutbox(ctx, tx, a, "GATEWAY_AUTHORIZATION_DECIDED"); err != nil {
		return err
	}
	return backupAdmissionAudit(ctx, tx, requestID, a.RepositoryID, "GATEWAY_AUTHORIZATION_DECISION", "REVOKED", domain.AuditSuccess)
}
