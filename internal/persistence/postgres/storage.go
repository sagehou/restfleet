package postgres

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sagehou/restfleet/internal/domain"
)

const credentialColumns = "id, name, provider, remote_name, status, secret_ref, secret_revision, revision, created_at, updated_at"

func scanCredential(row rowScanner) (domain.StorageCredential, error) {
	var c domain.StorageCredential
	err := row.Scan(&c.ID, &c.Name, &c.Provider, &c.RemoteName, &c.Status,
		&c.SecretRef, &c.SecretRevision, &c.Revision, &c.CreatedAt, &c.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return c, domain.ErrNotFound
	}
	return c, err
}

func (s *Store) StorageCredentials(ctx context.Context, after uuid.UUID, limit int) ([]domain.StorageCredential, error) {
	rows, err := s.pool.Query(ctx, "select "+credentialColumns+" from storage_credentials where id > $1 order by id limit $2", after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]domain.StorageCredential, 0)
	for rows.Next() {
		c, err := scanCredential(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, c)
	}
	return items, rows.Err()
}

func (s *Store) StorageCredential(ctx context.Context, id uuid.UUID) (domain.StorageCredential, error) {
	return scanCredential(s.pool.QueryRow(ctx, "select "+credentialColumns+" from storage_credentials where id = $1", id))
}

// StorageCredentialSecret is an explicit secret access port. Metadata reads never join secrets.
func (s *Store) StorageCredentialSecret(ctx context.Context, id uuid.UUID) (domain.SecretEnvelope, error) {
	var e domain.SecretEnvelope
	err := s.pool.QueryRow(ctx, `
		select s.id, s.kind, s.algorithm, s.key_id, s.ciphertext, s.nonce,
		       s.wrapped_data_key, s.wrap_nonce, s.aad, s.created_at
		from secrets s
		join storage_credential_revisions r on r.secret_ref = s.id
		where s.id = $1
	`, id).Scan(&e.ID, &e.Kind, &e.Algorithm, &e.KeyID, &e.Ciphertext,
		&e.Nonce, &e.WrappedDataKey, &e.WrapNonce, &e.AAD, &e.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return e, domain.ErrNotFound
	}
	return e, err
}

func insertStorageSecret(ctx context.Context, tx pgx.Tx, e domain.SecretEnvelope) error {
	_, err := tx.Exec(ctx, `
		insert into secrets (id, kind, algorithm, key_id, ciphertext, nonce,
			wrapped_data_key, wrap_nonce, aad, created_at)
		values ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
	`, e.ID, e.Kind, e.Algorithm, e.KeyID, e.Ciphertext, e.Nonce,
		e.WrappedDataKey, e.WrapNonce, e.AAD, e.CreatedAt)
	return err
}

// SaveStorageCredential keeps the metadata, immutable encrypted revision and audit atomic.
// expectedRevision == 0 creates; a nil secret only updates status.
func (s *Store) SaveStorageCredential(
	ctx context.Context, c domain.StorageCredential, expectedRevision int64,
	secret *domain.SecretEnvelope, audit domain.AuditEvent,
) (domain.StorageCredential, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return domain.StorageCredential{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if expectedRevision > 0 {
		current, err := scanCredential(tx.QueryRow(ctx, "select "+credentialColumns+" from storage_credentials where id = $1 for update", c.ID))
		if err != nil {
			return domain.StorageCredential{}, err
		}
		if current.Revision != expectedRevision {
			return domain.StorageCredential{}, domain.ErrRevisionConflict
		}
	}
	if secret != nil {
		if err := insertStorageSecret(ctx, tx, *secret); err != nil {
			return domain.StorageCredential{}, err
		}
	}
	if expectedRevision == 0 {
		_, err = tx.Exec(ctx, `
			insert into storage_credentials (`+credentialColumns+`)
			values ($1,$2,$3,$4,$5,$6,$7,1,$8,$8)
		`, c.ID, c.Name, c.Provider, c.RemoteName, c.Status, c.SecretRef, c.SecretRevision, c.CreatedAt)
	} else {
		_, err = tx.Exec(ctx, `
			update storage_credentials set status=$2, secret_ref=$3, secret_revision=$4,
			    revision=revision+1, updated_at=$5 where id=$1
		`, c.ID, c.Status, c.SecretRef, c.SecretRevision, c.UpdatedAt)
	}
	if err != nil {
		return domain.StorageCredential{}, persistenceError(err)
	}
	if secret != nil {
		_, err = tx.Exec(ctx, `
			insert into storage_credential_revisions (credential_id, revision, secret_ref, created_at)
			values ($1,$2,$3,$4)
		`, c.ID, c.SecretRevision, c.SecretRef, c.UpdatedAt)
		if err != nil {
			return domain.StorageCredential{}, err
		}
	}
	if err := appendAudit(ctx, tx, audit); err != nil {
		return domain.StorageCredential{}, err
	}
	c.Revision = expectedRevision + 1
	return c, tx.Commit(ctx)
}
