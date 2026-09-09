package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/sagehou/restfleet/internal/domain"
)

// BackupAdmissionMaterial releases ciphertext only after the exact current
// admission and a committed secret-access audit. Metadata APIs never call it.
func (s *Store) BackupAdmissionMaterial(ctx context.Context, id, owner uuid.UUID, hash string) (domain.BackupMaterial, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return domain.BackupMaterial{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	a, err := lockBackupAdmission(ctx, tx, id, owner, hash)
	if errors.Is(err, domain.ErrBackupAdmission) {
		err = rejectBackupAdmission(ctx, tx, id, uuid.Nil, err)
	}
	if err != nil {
		return domain.BackupMaterial{}, err
	}
	c, err := scanCredential(tx.QueryRow(ctx, "select "+credentialColumns+" from storage_credentials where id=$1", a.StorageCredentialID))
	if err != nil {
		return domain.BackupMaterial{}, err
	}
	var e domain.SecretEnvelope
	err = tx.QueryRow(ctx, `select s.id,s.kind,s.algorithm,s.key_id,s.ciphertext,s.nonce,s.wrapped_data_key,s.wrap_nonce,s.aad,s.created_at
		from secrets s join storage_credential_revisions r on r.secret_ref=s.id
		where s.id=$1 and r.credential_id=$2 and r.revision=$3`, c.SecretRef, c.ID, c.SecretRevision).
		Scan(&e.ID, &e.Kind, &e.Algorithm, &e.KeyID, &e.Ciphertext, &e.Nonce, &e.WrappedDataKey, &e.WrapNonce, &e.AAD, &e.CreatedAt)
	if err != nil {
		return domain.BackupMaterial{}, err
	}
	if err = backupAdmissionAudit(ctx, tx, id, a.RepositoryID, "GATEWAY_STORAGE_SECRET_ACCESS", "AUTHORIZED", domain.AuditSuccess); err != nil {
		return domain.BackupMaterial{}, err
	}
	if err = ensureLiveBackupAdmission(ctx, tx, id, owner); err != nil {
		return domain.BackupMaterial{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return domain.BackupMaterial{}, err
	}
	return domain.BackupMaterial{Admission: a, Credential: c, Envelope: e}, nil
}

// RefreshBackupAdmission is exclusively for the admitted central config
// owner. The central service MUST validate token-only changes and seal the next
// revision before calling. Other writers remain blocked by the same fence.
func (s *Store) RefreshBackupAdmission(ctx context.Context, id, owner uuid.UUID, hash string, expected int64, e domain.SecretEnvelope) (domain.StorageCredential, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return domain.StorageCredential{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	a, err := lockBackupAdmission(ctx, tx, id, owner, hash)
	if errors.Is(err, domain.ErrBackupAdmission) {
		err = rejectBackupAdmission(ctx, tx, id, uuid.Nil, err)
	}
	if err != nil {
		return domain.StorageCredential{}, err
	}
	c, err := scanCredential(tx.QueryRow(ctx, "select "+credentialColumns+" from storage_credentials where id=$1", a.StorageCredentialID))
	if err != nil {
		return domain.StorageCredential{}, err
	}
	if expected < 1 || c.SecretRevision != expected {
		return domain.StorageCredential{}, domain.ErrRevisionConflict
	}
	if e.Kind != "RCLONE_CONFIG" || e.ID.Version() != 7 || e.ID.Variant() != uuid.RFC4122 {
		return domain.StorageCredential{}, domain.ErrStorageUnavailable
	}
	var now time.Time
	if err = tx.QueryRow(ctx, "select clock_timestamp()").Scan(&now); err != nil {
		return domain.StorageCredential{}, err
	}
	if err = insertSecret(ctx, tx, e); err != nil {
		return domain.StorageCredential{}, err
	}
	if _, err = tx.Exec(ctx, `insert into storage_credential_revisions(credential_id,revision,secret_ref,created_at) values($1,$2,$3,$4)`, c.ID, expected+1, e.ID, now); err != nil {
		return domain.StorageCredential{}, err
	}
	c, err = scanCredential(tx.QueryRow(ctx, `update storage_credentials set secret_ref=$2,secret_revision=secret_revision+1,
		revision=revision+1,last_refreshed_at=$3,updated_at=$3 where id=$1 returning `+credentialColumns, c.ID, e.ID, now))
	if err != nil {
		return domain.StorageCredential{}, err
	}
	if err = backupAdmissionAudit(ctx, tx, id, a.RepositoryID, "GATEWAY_STORAGE_REFRESH", "SECRET_CHANGED", domain.AuditSuccess); err != nil {
		return domain.StorageCredential{}, err
	}
	if err = ensureLiveBackupAdmission(ctx, tx, id, owner); err != nil {
		return domain.StorageCredential{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return domain.StorageCredential{}, err
	}
	return c, nil
}

func ensureLiveBackupAdmission(ctx context.Context, tx pgx.Tx, id, owner uuid.UUID) error {
	var live bool
	if err := tx.QueryRow(ctx, "select exists(select 1 from gateway_backup_admissions where id=$1 and owner=$2 and released_at is null and expires_at>clock_timestamp())", id, owner).Scan(&live); err != nil {
		return err
	}
	if !live {
		return domain.ErrBackupAdmission
	}
	return nil
}
