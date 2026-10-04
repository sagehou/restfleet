package postgres

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/sagehou/restfleet/internal/domain"
)

func (s *Store) RegisterGatewayAuditOrigin(ctx context.Context, b domain.GatewayAuditBinding, public ed25519.PublicKey) (domain.GatewayAuditOrigin, error) {
	if b.Validate() != nil || len(public) != 32 {
		return domain.GatewayAuditOrigin{}, domain.ErrGatewayGlobalAudit
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return domain.GatewayAuditOrigin{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, "insert into gateway_audit_origins(id,runtime_id,public_key) values($1,$2,$3) on conflict(id) do nothing", b.OriginID, b.RuntimeID, public)
	if err != nil {
		return domain.GatewayAuditOrigin{}, err
	}
	o, err := scanAuditOrigin(tx.QueryRow(ctx, "select id,runtime_id,public_key,created_at,closed_at from gateway_audit_origins where id=$1 for update", b.OriginID))
	if err != nil || o.Binding != b || !bytes.Equal(o.PublicKey, public) || o.ClosedAt != nil {
		return domain.GatewayAuditOrigin{}, domain.ErrGatewayGlobalAudit
	}
	if tag.RowsAffected() == 1 {
		if err := appendAudit(ctx, tx, domain.AuditEvent{ID: b.OriginID, RequestID: b.OriginID, OccurredAt: o.CreatedAt,
			ActorType: domain.ActorSystem, Action: "GATEWAY_AUDIT_ORIGIN", ResourceType: "GATEWAY", Result: domain.AuditSuccess, ReasonCode: "REGISTERED"}); err != nil {
			return domain.GatewayAuditOrigin{}, err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return domain.GatewayAuditOrigin{}, err
	}
	return o, nil
}

func scanAuditOrigin(row pgx.Row) (domain.GatewayAuditOrigin, error) {
	var o domain.GatewayAuditOrigin
	err := row.Scan(&o.Binding.OriginID, &o.Binding.RuntimeID, &o.PublicKey, &o.CreatedAt, &o.ClosedAt)
	return o, err
}

func (s *Store) GatewayAuditOrigin(ctx context.Context, b domain.GatewayAuditBinding) (domain.GatewayAuditOrigin, error) {
	return scanAuditOrigin(s.pool.QueryRow(ctx, "select id,runtime_id,public_key,created_at,closed_at from gateway_audit_origins where id=$1 and runtime_id=$2", b.OriginID, b.RuntimeID))
}

func auditTail(ctx context.Context, tx pgx.Tx, b domain.GatewayAuditBinding) (int64, string, error) {
	var sequence int64
	var hash string
	err := tx.QueryRow(ctx, "select sequence,wire_hash from gateway_audit_records where origin_id=$1 and runtime_id=$2 order by sequence desc limit 1", b.OriginID, b.RuntimeID).Scan(&sequence, &hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, strings.Repeat("0", 64), nil
	}
	return sequence, hash, err
}

func (s *Store) CommitGatewayGlobalAudit(ctx context.Context, r domain.GatewayAuditCommit) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	o, err := scanAuditOrigin(tx.QueryRow(ctx, "select id,runtime_id,public_key,created_at,closed_at from gateway_audit_origins where id=$1 and runtime_id=$2 for update", r.Binding.OriginID, r.Binding.RuntimeID))
	if err != nil {
		return err
	}
	var oldID, oldHash string
	err = tx.QueryRow(ctx, "select record_id::text,wire_hash from gateway_audit_records where origin_id=$1 and runtime_id=$2 and sequence=$3", r.Binding.OriginID, r.Binding.RuntimeID, r.Sequence).Scan(&oldID, &oldHash)
	if err == nil {
		if oldID != r.RecordID.String() || oldHash != r.WireHash {
			return domain.ErrGatewayGlobalAudit
		}
		return tx.Commit(ctx) // Exact lost-ACK replay remains safe after sealing.
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	sequence, hash, err := auditTail(ctx, tx, r.Binding)
	if err != nil {
		return err
	}
	var now time.Time
	if err = tx.QueryRow(ctx, "select clock_timestamp()").Scan(&now); err != nil {
		return err
	}
	if o.ClosedAt != nil || r.Sequence != sequence+1 || r.PreviousHash != hash || r.CreatedAt.Before(o.CreatedAt.Truncate(time.Second)) || r.CreatedAt.After(now) ||
		r.Audit.ID != r.RecordID || r.Audit.RequestID != r.RecordID || !r.Audit.OccurredAt.Equal(r.CreatedAt) || r.Audit.ResourceID != [16]byte{} || r.Audit.ActorID != [16]byte{} ||
		r.Audit.ActorType != domain.ActorSystem || r.Audit.ResourceType != "GATEWAY" || r.Audit.Result != domain.AuditDenied {
		return domain.ErrGatewayGlobalAudit
	}
	if err = appendAudit(ctx, tx, r.Audit); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `insert into gateway_audit_records(origin_id,runtime_id,sequence,record_id,wire_hash,occurred_at)
		values($1,$2,$3,$4,$5,$6)`, r.Binding.OriginID, r.Binding.RuntimeID, r.Sequence, r.RecordID, r.WireHash, r.CreatedAt); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Only the trusted coordinator calls this after public ingress and every
// producer have joined and the exact queue tail has been centrally committed.
func (s *Store) CloseGatewayAuditOrigin(ctx context.Context, b domain.GatewayAuditBinding, sequence int64, hash string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	o, err := scanAuditOrigin(tx.QueryRow(ctx, "select id,runtime_id,public_key,created_at,closed_at from gateway_audit_origins where id=$1 and runtime_id=$2 for update", b.OriginID, b.RuntimeID))
	if err != nil {
		return err
	}
	actual, actualHash, err := auditTail(ctx, tx, b)
	if err != nil || actual != sequence || actualHash != hash {
		return domain.ErrGatewayGlobalAudit
	}
	if o.ClosedAt != nil {
		return tx.Commit(ctx)
	}
	if _, err = tx.Exec(ctx, "update gateway_audit_origins set closed_at=clock_timestamp() where id=$1", b.OriginID); err != nil {
		return err
	}
	if err = appendAudit(ctx, tx, domain.AuditEvent{RequestID: b.OriginID, OccurredAt: time.Now().UTC(), ActorType: domain.ActorSystem,
		Action: "GATEWAY_AUDIT_ORIGIN", ResourceType: "GATEWAY", Result: domain.AuditSuccess, ReasonCode: "SEALED"}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
