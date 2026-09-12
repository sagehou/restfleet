package postgres

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/sagehou/restfleet/internal/domain"
)

const writebackColumns = "id,authorization_id,owner,gateway_instance_id,sequence,record_type,payload,checksum,created_at,confirmed_at"

func scanWriteback(row rowScanner) (domain.WritebackRecord, error) {
	var r domain.WritebackRecord
	err := row.Scan(&r.ID, &r.AuthorizationID, &r.Owner, &r.GatewayInstanceID,
		&r.Sequence, &r.RecordType, &r.Payload, &r.Checksum, &r.CreatedAt, &r.ConfirmedAt)
	return r, err
}

// SubmitWritebackRecord persists a Gateway write-back record. The authorization
// MUST be active and the sequence MUST be strictly greater than the last
// confirmed record for this gateway instance. Payload is already encrypted by
// the Gateway; the server stores it opaque.
func (s *Store) SubmitWritebackRecord(ctx context.Context, submission domain.WritebackSubmission) (domain.WritebackRecord, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return domain.WritebackRecord{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Verify authorization is still active.
	var authActive bool
	err = tx.QueryRow(ctx,
		`select exists(select 1 from offline_authorizations
		 where id=$1 and owner=$2 and revoked_at is null and disabled_at is null and expires_at>clock_timestamp())`,
		submission.AuthorizationID, submission.Owner).Scan(&authActive)
	if err != nil {
		return domain.WritebackRecord{}, err
	}
	if !authActive {
		return domain.WritebackRecord{}, domain.ErrOfflineAuthorization
	}

	// Check sequence ordering: must be > last confirmed.
	var lastSeq int64
	_ = tx.QueryRow(ctx,
		`select coalesce(max(sequence), 0) from writeback_records where gateway_instance_id=$1 and confirmed_at is not null`,
		submission.GatewayInstanceID).Scan(&lastSeq)
	if submission.Sequence <= lastSeq {
		return domain.WritebackRecord{}, domain.ErrAntiRollback
	}

	// Check for exact duplicate (idempotent replay).
	existing, err := scanWriteback(tx.QueryRow(ctx,
		"select "+writebackColumns+" from writeback_records where id=$1", submission.RecordID))
	if err == nil {
		return existing, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return domain.WritebackRecord{}, err
	}

	r := domain.WritebackRecord{
		ID:                submission.RecordID,
		AuthorizationID:   submission.AuthorizationID,
		Owner:             submission.Owner,
		GatewayInstanceID: submission.GatewayInstanceID,
		Sequence:          submission.Sequence,
		RecordType:        submission.RecordType,
		Payload:           submission.Payload,
		Checksum:          submission.Checksum,
		CreatedAt:         submission.CreatedAt,
	}

	_, err = tx.Exec(ctx,
		`insert into writeback_records(`+writebackColumns+`)
		 values($1,$2,$3,$4,$5,$6,$7,$8,$9,null)`,
		r.ID, r.AuthorizationID, r.Owner, r.GatewayInstanceID,
		r.Sequence, r.RecordType, r.Payload, r.Checksum, r.CreatedAt)
	if err != nil {
		return domain.WritebackRecord{}, err
	}

	return r, tx.Commit(ctx)
}

// ConfirmWritebackRecord marks a record as confirmed. Idempotent.
func (s *Store) ConfirmWritebackRecord(ctx context.Context, recordID uuid.UUID) error {
	_, err := s.pool.Exec(ctx,
		`update writeback_records set confirmed_at=clock_timestamp()
		 where id=$1 and confirmed_at is null`, recordID)
	return err
}

// PendingWritebackRecords returns unconfirmed records for a gateway instance,
// ordered by sequence. Used during recovery to replay unconfirmed records.
func (s *Store) PendingWritebackRecords(ctx context.Context, gatewayInstanceID uuid.UUID, limit int) ([]domain.WritebackRecord, error) {
	rows, err := s.pool.Query(ctx,
		"select "+writebackColumns+" from writeback_records where gateway_instance_id=$1 and confirmed_at is null order by sequence limit $2",
		gatewayInstanceID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var records []domain.WritebackRecord
	for rows.Next() {
		r, err := scanWriteback(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, r)
	}
	return records, rows.Err()
}
