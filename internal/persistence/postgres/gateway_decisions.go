package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/sagehou/restfleet/internal/domain"
)

// DecideGatewayAuthorization serializes all revisions on the original admission
// row. It never releases/reacquires or extends that fence and never signs data.
func (s *Store) DecideGatewayAuthorization(ctx context.Context, r domain.GatewayDecisionRequest, hash string) (domain.GatewayDecision, error) {
	if err := r.Validate(); err != nil {
		return domain.GatewayDecision{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return domain.GatewayDecision{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var a domain.BackupAdmission
	if r.Revoke {
		// Explicit withdrawal must remain possible after identity/credential
		// disable or expiry. Only immutable admission ID/owner is required.
		a, err = scanBackupAdmission(tx.QueryRow(ctx, "select "+admissionColumns+" from gateway_backup_admissions where id=$1 and owner=$2 for update", r.AdmissionID, r.Owner))
	} else {
		a, err = lockBackupAdmission(ctx, tx, r.AdmissionID, r.Owner, hash)
	}
	deny := func() (domain.GatewayDecision, error) {
		if err := backupAdmissionAudit(ctx, tx, r.ID, a.RepositoryID, "GATEWAY_AUTHORIZATION_DENIED", "REJECTED", domain.AuditDenied); err != nil {
			return domain.GatewayDecision{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return domain.GatewayDecision{}, err
		}
		return domain.GatewayDecision{}, domain.ErrGatewayDecision
	}
	if errors.Is(err, domain.ErrBackupAdmission) || errors.Is(err, pgx.ErrNoRows) {
		return deny()
	}
	if err != nil {
		return domain.GatewayDecision{}, err
	}
	var previous domain.GatewayDecision
	var expires *time.Time
	var seconds int64
	err = tx.QueryRow(ctx, `select request_id,runtime_id,revision,lifetime_seconds,issued_at,expires_at,revoked
		from gateway_authorization_decisions where admission_id=$1 order by revision desc limit 1`, a.ID).
		Scan(&previous.RequestID, &previous.RuntimeID, &previous.Revision, &seconds, &previous.IssuedAt, &expires, &previous.Revoked)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return domain.GatewayDecision{}, err
	}
	if expires != nil {
		previous.ExpiresAt = *expires
	}
	previous.RequestedLifetime = time.Duration(seconds) * time.Second
	previous.Admission = a
	if previous.Revision > 0 && previous.RuntimeID != r.RuntimeID {
		return deny()
	}
	var decision domain.GatewayDecision
	replay := previous.RequestID == r.ID
	if replay {
		if previous.Revision != r.ExpectedRevision+1 || previous.Revoked != r.Revoke || previous.RequestedLifetime != r.Lifetime {
			return deny()
		}
		decision = previous
	} else {
		if previous.Revision != r.ExpectedRevision || previous.Revoked || a.ReleasedAt != nil {
			return deny()
		}
		var used bool
		if err = tx.QueryRow(ctx, "select exists(select 1 from gateway_authorization_decisions where request_id=$1)", r.ID).Scan(&used); err != nil {
			return domain.GatewayDecision{}, err
		}
		if used {
			return deny()
		}
		var now time.Time
		if err = tx.QueryRow(ctx, "select date_trunc('second',clock_timestamp())").Scan(&now); err != nil {
			return domain.GatewayDecision{}, err
		}
		if now.Before(previous.IssuedAt) {
			return deny()
		}
		decision = domain.GatewayDecision{RequestID: r.ID, RuntimeID: r.RuntimeID, Admission: a, Revision: r.ExpectedRevision + 1,
			RequestedLifetime: r.Lifetime, IssuedAt: now, Revoked: r.Revoke}
		var expiry any
		if !r.Revoke {
			decision.ExpiresAt = now.Add(r.Lifetime)
			limit := a.ExpiresAt.Truncate(time.Second)
			if decision.ExpiresAt.After(limit) {
				decision.ExpiresAt = limit
			}
			if !decision.ExpiresAt.After(now) {
				return deny()
			}
			expiry = decision.ExpiresAt
		}
		_, err = tx.Exec(ctx, `insert into gateway_authorization_decisions(admission_id,revision,request_id,runtime_id,lifetime_seconds,issued_at,expires_at,revoked)
			values($1,$2,$3,$4,$5,$6,$7,$8)`, a.ID, decision.Revision, r.ID, r.RuntimeID, int64(r.Lifetime/time.Second), now, expiry, r.Revoke)
		if err != nil {
			return domain.GatewayDecision{}, err
		}
		if err = admissionOutbox(ctx, tx, a, "GATEWAY_AUTHORIZATION_DECIDED"); err != nil {
			return domain.GatewayDecision{}, err
		}
		reason := "GRANTED"
		if r.Revoke {
			reason = "REVOKED"
		}
		if err = backupAdmissionAudit(ctx, tx, r.ID, a.RepositoryID, "GATEWAY_AUTHORIZATION_DECISION", reason, domain.AuditSuccess); err != nil {
			return domain.GatewayDecision{}, err
		}
	}
	if !decision.Revoked {
		if err = ensureLiveBackupAdmission(ctx, tx, a.ID, a.Owner); err != nil {
			return domain.GatewayDecision{}, err
		}
		var live bool
		if err = tx.QueryRow(ctx, "select $1::timestamptz>clock_timestamp()", decision.ExpiresAt).Scan(&live); err != nil {
			return domain.GatewayDecision{}, err
		}
		if !live {
			return domain.GatewayDecision{}, domain.ErrGatewayDecision
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return domain.GatewayDecision{}, err
	}
	return decision, nil
}

// All callers hold the admission row lock. Known withdrawal blocks online
// checks/material refresh too; connection errors never write such a decision.
func ensureGatewayAuthorizationNotRevoked(ctx context.Context, tx pgx.Tx, id uuid.UUID) error {
	var revoked bool
	if err := tx.QueryRow(ctx, "select exists(select 1 from gateway_authorization_decisions where admission_id=$1 and revoked)", id).Scan(&revoked); err != nil {
		return err
	}
	if revoked {
		return domain.ErrBackupAdmission
	}
	return nil
}

// Cleanup acknowledgement cannot silently discard outstanding signed grants.
// Explicit withdrawal is still not proof that a disconnected process stopped:
// the release caller must ALSO prove all data-plane and persistence cleanup.
func ensureGatewayAuthorizationReleasable(ctx context.Context, tx pgx.Tx, id uuid.UUID) error {
	var blocked bool
	err := tx.QueryRow(ctx, `select exists(select 1 from gateway_authorization_decisions where admission_id=$1 and not revoked and expires_at>clock_timestamp())
		and not exists(select 1 from gateway_authorization_decisions where admission_id=$1 and revoked)`, id).Scan(&blocked)
	if err != nil {
		return err
	}
	if blocked {
		return domain.ErrGatewayDecision
	}
	return nil
}
