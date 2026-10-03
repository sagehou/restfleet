package postgres

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/sagehou/restfleet/internal/domain"
)

// RegisterGatewayPendingOrigin requires a current committed authorization and
// the existing online checks. It cannot rekey or take over an old origin.
func (s *Store) RegisterGatewayPendingOrigin(ctx context.Context, id, owner, runtime uuid.UUID, public ed25519.PublicKey, hash string) (domain.GatewayPendingOrigin, error) {
	if runtime.Version() != 7 || runtime.Variant() != uuid.RFC4122 || len(public) != 32 {
		return domain.GatewayPendingOrigin{}, domain.ErrGatewayPending
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return domain.GatewayPendingOrigin{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	a, err := lockBackupAdmission(ctx, tx, id, owner, hash)
	if err != nil {
		return domain.GatewayPendingOrigin{}, err
	}
	var valid bool
	err = tx.QueryRow(ctx, `select exists(select 1 from gateway_authorization_decisions where admission_id=$1 and runtime_id=$2 and not revoked and expires_at>clock_timestamp())`, id, runtime).Scan(&valid)
	if err != nil || !valid {
		return domain.GatewayPendingOrigin{}, domain.ErrGatewayPending
	}
	var revision int64
	if err = tx.QueryRow(ctx, "select secret_revision from storage_credentials where id=$1", a.StorageCredentialID).Scan(&revision); err != nil {
		return domain.GatewayPendingOrigin{}, err
	}
	var existing domain.GatewayPendingOrigin
	existing.Admission = a
	err = tx.QueryRow(ctx, `select runtime_id,public_key,initial_secret_revision,closed_at from gateway_pending_origins where admission_id=$1`, id).Scan(&existing.RuntimeID, &existing.PublicKey, &existing.InitialSecretRevision, &existing.ClosedAt)
	if err == nil {
		if existing.RuntimeID != runtime || !bytes.Equal(existing.PublicKey, public) || existing.ClosedAt != nil {
			return domain.GatewayPendingOrigin{}, domain.ErrGatewayPending
		}
		return existing, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return domain.GatewayPendingOrigin{}, err
	}
	_, err = tx.Exec(ctx, `insert into gateway_pending_origins(runtime_id,admission_id,public_key,initial_secret_revision) values($1,$2,$3,$4)`, runtime, id, []byte(public), revision)
	if err != nil {
		return domain.GatewayPendingOrigin{}, err
	}
	if err = backupAdmissionAudit(ctx, tx, runtime, a.RepositoryID, "GATEWAY_PENDING_ORIGIN", "REGISTERED", domain.AuditSuccess); err != nil {
		return domain.GatewayPendingOrigin{}, err
	}
	if err = ensureLiveBackupAdmission(ctx, tx, id, owner); err != nil {
		return domain.GatewayPendingOrigin{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return domain.GatewayPendingOrigin{}, err
	}
	return domain.GatewayPendingOrigin{Admission: a, RuntimeID: runtime, PublicKey: append(ed25519.PublicKey(nil), public...), InitialSecretRevision: revision}, nil
}

func (s *Store) GatewayPendingOrigin(ctx context.Context, id, runtime uuid.UUID) (domain.GatewayPendingOrigin, error) {
	var o domain.GatewayPendingOrigin
	var admissionID uuid.UUID
	err := s.pool.QueryRow(ctx, `select runtime_id,admission_id,public_key,initial_secret_revision,closed_at from gateway_pending_origins where admission_id=$1 and runtime_id=$2`, id, runtime).Scan(&o.RuntimeID, &admissionID, &o.PublicKey, &o.InitialSecretRevision, &o.ClosedAt)
	if err != nil {
		return o, err
	}
	o.Admission, err = scanBackupAdmission(s.pool.QueryRow(ctx, "select "+admissionColumns+" from gateway_backup_admissions where id=$1", admissionID))
	return o, err
}

// Replay locking deliberately does not require ACTIVE principals or a live
// authorization: old observations/refreshes must survive expiry and revocation.
// It does require the ORIGINAL unreleased fence and registered immutable owner.
func lockPendingOrigin(ctx context.Context, tx pgx.Tx, id, runtime uuid.UUID) (domain.GatewayPendingOrigin, error) {
	var o domain.GatewayPendingOrigin
	a, err := scanBackupAdmission(tx.QueryRow(ctx, "select "+admissionColumns+" from gateway_backup_admissions where id=$1", id))
	if err != nil {
		return o, err
	}
	// Same writer lock as replacement and refresh. Expired fences never disappear.
	if _, err = tx.Exec(ctx, "select id from storage_credentials where id=$1 for update", a.StorageCredentialID); err != nil {
		return o, err
	}
	o.Admission, err = scanBackupAdmission(tx.QueryRow(ctx, "select "+admissionColumns+" from gateway_backup_admissions where id=$1 for update", id))
	if err != nil {
		return o, err
	}
	err = tx.QueryRow(ctx, "select runtime_id,public_key,initial_secret_revision,closed_at from gateway_pending_origins where admission_id=$1 and runtime_id=$2 for update", id, runtime).Scan(&o.RuntimeID, &o.PublicKey, &o.InitialSecretRevision, &o.ClosedAt)
	return o, err
}

func pendingTail(ctx context.Context, tx pgx.Tx, id, runtime uuid.UUID) (int64, string, error) {
	var sequence int64
	var hash string
	err := tx.QueryRow(ctx, "select sequence,wire_hash from gateway_pending_records where admission_id=$1 and runtime_id=$2 order by sequence desc limit 1", id, runtime).Scan(&sequence, &hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, strings.Repeat("0", 64), nil
	}
	return sequence, hash, err
}

// CommitGatewayPending atomically writes effect and receipt history. validate
// is CENTRAL ONLY, runs under the credential lock, and returns encrypted config.
func (s *Store) CommitGatewayPending(ctx context.Context, r domain.GatewayPendingCommit,
	validate func(domain.StorageCredential, domain.SecretEnvelope) (domain.SecretEnvelope, error),
) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	o, err := lockPendingOrigin(ctx, tx, r.AdmissionID, r.RuntimeID)
	if err != nil {
		return err
	}
	a := o.Admission
	if a.ID != r.AdmissionID || a.Owner != r.Owner {
		return domain.ErrGatewayPending
	}
	var oldID uuid.UUID
	var oldHash string
	err = tx.QueryRow(ctx, "select record_id,wire_hash from gateway_pending_records where admission_id=$1 and runtime_id=$2 and sequence=$3", r.AdmissionID, r.RuntimeID, r.Sequence).Scan(&oldID, &oldHash)
	if err == nil {
		if oldID != r.RecordID || oldHash != r.WireHash {
			return domain.ErrGatewayPending
		}
		return tx.Commit(ctx) // ACK-loss replay, also safe after central sealing.
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if a.ReleasedAt != nil || o.ClosedAt != nil {
		return domain.ErrGatewayPending
	}
	sequence, hash, err := pendingTail(ctx, tx, r.AdmissionID, r.RuntimeID)
	if err != nil {
		return err
	}
	if r.Sequence != sequence+1 || r.PreviousHash != hash || r.CreatedAt.Before(a.CreatedAt.Truncate(time.Second)) {
		return domain.ErrGatewayPending
	}
	var issued, expires time.Time
	err = tx.QueryRow(ctx, `select issued_at,expires_at from gateway_authorization_decisions where admission_id=$1 and runtime_id=$2 and revision=$3 and not revoked`, a.ID, r.RuntimeID, r.AuthorizationRevision).Scan(&issued, &expires)
	if err != nil || r.CreatedAt.Before(issued) {
		return domain.ErrGatewayPending
	}
	var now time.Time
	if err = tx.QueryRow(ctx, "select clock_timestamp()").Scan(&now); err != nil {
		return err
	}
	if r.CreatedAt.After(now) {
		return domain.ErrGatewayPending
	}
	switch r.Kind {
	case "audit":
		if r.Audit == nil || r.ExpectedSecretRevision != 0 || validate != nil {
			return domain.ErrGatewayPending
		}
		if err = appendAudit(ctx, tx, *r.Audit); err != nil {
			return err
		}
	case "refresh":
		if r.Audit != nil || validate == nil || !r.CreatedAt.Before(expires) {
			return domain.ErrGatewayPending
		}
		c, err := scanCredential(tx.QueryRow(ctx, "select "+credentialColumns+" from storage_credentials where id=$1", a.StorageCredentialID))
		if err != nil {
			return err
		}
		if c.SecretRevision != r.ExpectedSecretRevision || c.SecretRevision < o.InitialSecretRevision {
			return domain.ErrRevisionConflict
		}
		var envelope domain.SecretEnvelope
		err = tx.QueryRow(ctx, `select id,kind,algorithm,key_id,ciphertext,nonce,wrapped_data_key,wrap_nonce,aad,created_at from secrets where id=$1`, c.SecretRef).Scan(&envelope.ID, &envelope.Kind, &envelope.Algorithm, &envelope.KeyID, &envelope.Ciphertext, &envelope.Nonce, &envelope.WrappedDataKey, &envelope.WrapNonce, &envelope.AAD, &envelope.CreatedAt)
		if err != nil {
			return err
		}
		next, err := validate(c, envelope)
		if err != nil || next.Kind != "RCLONE_CONFIG" || next.ID.Version() != 7 || next.ID.Variant() != uuid.RFC4122 {
			return domain.ErrGatewayPending
		}
		if err = insertSecret(ctx, tx, next); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, "insert into storage_credential_revisions(credential_id,revision,secret_ref,created_at) values($1,$2,$3,$4)", c.ID, c.SecretRevision+1, next.ID, now); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `update storage_credentials set secret_ref=$2,secret_revision=secret_revision+1,revision=revision+1,last_refreshed_at=$3,updated_at=$3 where id=$1`, c.ID, next.ID, now); err != nil {
			return err
		}
		if err = backupAdmissionAudit(ctx, tx, r.RecordID, a.RepositoryID, "GATEWAY_PENDING_REFRESH", "SECRET_CHANGED", domain.AuditSuccess); err != nil {
			return err
		}
	default:
		return domain.ErrGatewayPending
	}
	_, err = tx.Exec(ctx, `insert into gateway_pending_records(admission_id,runtime_id,sequence,record_id,wire_hash,authorization_revision,kind,occurred_at) values($1,$2,$3,$4,$5,$6,$7,$8)`, r.AdmissionID, r.RuntimeID, r.Sequence, r.RecordID, r.WireHash, r.AuthorizationRevision, r.Kind, r.CreatedAt)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// CloseGatewayPendingOrigin is only for a trusted cleanup coordinator AFTER
// joining appenders/processes and replaying the exact tail. It proves neither.
func (s *Store) CloseGatewayPendingOrigin(ctx context.Context, id, owner, runtime uuid.UUID, sequence int64, hash string) error {
	b, err := hex.DecodeString(hash)
	if sequence < 0 || err != nil || len(b) != 32 || hex.EncodeToString(b) != hash {
		return domain.ErrGatewayPending
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	o, err := lockPendingOrigin(ctx, tx, id, runtime)
	if err != nil {
		return err
	}
	actual, actualHash, err := pendingTail(ctx, tx, id, runtime)
	if err != nil {
		return err
	}
	if o.Admission.ID != id || o.Admission.Owner != owner || actual != sequence || actualHash != hash {
		return domain.ErrGatewayPending
	}
	if o.ClosedAt != nil {
		return tx.Commit(ctx)
	}
	if o.Admission.ReleasedAt != nil {
		return domain.ErrGatewayPending
	}
	if _, err = tx.Exec(ctx, "update gateway_pending_origins set closed_at=clock_timestamp() where admission_id=$1 and runtime_id=$2", id, runtime); err != nil {
		return err
	}
	if err = backupAdmissionAudit(ctx, tx, runtime, o.Admission.RepositoryID, "GATEWAY_PENDING_ORIGIN", "SEALED", domain.AuditSuccess); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func ensureGatewayPendingReleasable(ctx context.Context, tx pgx.Tx, id uuid.UUID) error {
	var blocked bool
	if err := tx.QueryRow(ctx, "select exists(select 1 from gateway_pending_origins where admission_id=$1 and closed_at is null)", id).Scan(&blocked); err != nil {
		return err
	}
	if blocked {
		return domain.ErrGatewayPending
	}
	return nil
}
