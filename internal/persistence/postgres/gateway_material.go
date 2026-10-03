package postgres

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"time"

	"github.com/google/uuid"
	"github.com/sagehou/restfleet/internal/domain"
)

// GatewayDeliveryMaterial is a CENTRAL-only secret access transaction. Pinned
// source is supplied by trusted runtime provisioning, never selected by a
// request. Returning committed ciphertext does not authorize another delivery
// after the Gateway starts producing records or prove cleanup.
func (s *Store) GatewayDeliveryMaterial(ctx context.Context, id, owner, runtime uuid.UUID, source ed25519.PublicKey, hash, challengeHash string) (domain.GatewayDeliveryMaterial, error) {
	var result domain.GatewayDeliveryMaterial
	h, decodeErr := hex.DecodeString(challengeHash)
	if runtime.Version() != 7 || runtime.Variant() != uuid.RFC4122 || len(source) != 32 || decodeErr != nil || len(h) != 32 || hex.EncodeToString(h) != challengeHash {
		return result, domain.ErrGatewayPending
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return result, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	a, err := lockBackupAdmission(ctx, tx, id, owner, hash)
	if err != nil {
		return result, err
	}
	o := domain.GatewayPendingOrigin{Admission: a}
	err = tx.QueryRow(ctx, `select runtime_id,public_key,initial_secret_revision,closed_at
		from gateway_pending_origins where admission_id=$1 and runtime_id=$2 for update`, id, runtime).
		Scan(&o.RuntimeID, &o.PublicKey, &o.InitialSecretRevision, &o.ClosedAt)
	if err != nil || o.ClosedAt != nil || !bytes.Equal(o.PublicKey, source) {
		return result, domain.ErrGatewayPending
	}
	var started bool
	if err = tx.QueryRow(ctx, "select exists(select 1 from gateway_pending_records where admission_id=$1)", id).Scan(&started); err != nil || started {
		return result, domain.ErrGatewayPending
	}
	d := domain.GatewayDecision{Admission: a}
	var expiry *time.Time
	var lifetime int64
	err = tx.QueryRow(ctx, `select request_id,runtime_id,revision,lifetime_seconds,issued_at,expires_at,revoked
		from gateway_authorization_decisions where admission_id=$1 order by revision desc limit 1`, id).
		Scan(&d.RequestID, &d.RuntimeID, &d.Revision, &lifetime, &d.IssuedAt, &expiry, &d.Revoked)
	if err != nil || d.RuntimeID != runtime || d.Revoked || expiry == nil {
		return result, domain.ErrGatewayPending
	}
	d.ExpiresAt, d.RequestedLifetime = *expiry, time.Duration(lifetime)*time.Second
	c, err := scanCredential(tx.QueryRow(ctx, "select "+credentialColumns+" from storage_credentials where id=$1", a.StorageCredentialID))
	if err != nil || c.SecretRevision != o.InitialSecretRevision {
		return result, domain.ErrGatewayPending
	}
	var envelope domain.SecretEnvelope
	err = tx.QueryRow(ctx, `select s.id,s.kind,s.algorithm,s.key_id,s.ciphertext,s.nonce,s.wrapped_data_key,s.wrap_nonce,s.aad,s.created_at
		from secrets s join storage_credential_revisions r on r.secret_ref=s.id
		where s.id=$1 and r.credential_id=$2 and r.revision=$3`, c.SecretRef, c.ID, c.SecretRevision).
		Scan(&envelope.ID, &envelope.Kind, &envelope.Algorithm, &envelope.KeyID, &envelope.Ciphertext, &envelope.Nonce, &envelope.WrappedDataKey, &envelope.WrapNonce, &envelope.AAD, &envelope.CreatedAt)
	if err != nil {
		return result, err
	}
	// Commit a single delivery intent before any plaintext/encrypted wire can
	// leave the center. Even exact retry is uncertain after transport/ACK loss;
	// neither a fresh challenge nor drain permits replaying old material.
	_, err = tx.Exec(ctx, `insert into gateway_material_deliveries(admission_id,runtime_id,challenge_hash,authorization_revision,secret_revision)
		values($1,$2,$3,$4,$5)`, id, runtime, challengeHash, d.Revision, c.SecretRevision)
	if err != nil {
		return result, err
	}
	if err = backupAdmissionAudit(ctx, tx, id, a.RepositoryID, "GATEWAY_MATERIAL_DELIVERY", "SECRET_ACCESS", domain.AuditSuccess); err != nil {
		return result, err
	}
	var live bool
	err = tx.QueryRow(ctx, "select $1::timestamptz>clock_timestamp() and $2::timestamptz>clock_timestamp()", d.ExpiresAt, a.ExpiresAt).Scan(&live)
	if err != nil || !live {
		return result, domain.ErrGatewayPending
	}
	if err = tx.Commit(ctx); err != nil {
		return result, err
	}
	return domain.GatewayDeliveryMaterial{Material: domain.BackupMaterial{Admission: a, Credential: c, Envelope: envelope}, Origin: o, Decision: d}, nil
}
