package postgres

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/sagehou/restfleet/internal/domain"
)

func (s *Store) AppendOperationLogs(ctx context.Context, agentID, id uuid.UUID, entries []domain.BackupLogEntry) (uint64, error) {
	if len(entries) == 0 || len(entries) > 200 {
		return 0, domain.ErrOperationTransition
	}
	for i, entry := range entries {
		if entry.Validate() != nil || i > 0 && entry.Sequence != entries[i-1].Sequence+1 {
			return 0, domain.ErrOperationTransition
		}
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var owner uuid.UUID
	if err := tx.QueryRow(ctx, "select agent_id from backup_operations where id=$1 for update", id).Scan(&owner); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, domain.ErrPlanBindingInvalid
		}
		return 0, err
	}
	if owner != agentID {
		return 0, domain.ErrPlanBindingInvalid
	}
	if _, err := tx.Exec(ctx, "insert into backup_operation_log_heads(operation_id) values($1) on conflict do nothing", id); err != nil {
		return 0, err
	}
	var ack, used uint64
	if err := tx.QueryRow(ctx, "select ack_sequence,bytes_received from backup_operation_log_heads where operation_id=$1 for update", id).Scan(&ack, &used); err != nil {
		return 0, err
	}
	if entries[0].Sequence > ack+1 {
		return 0, domain.ErrOperationTransition
	}
	for _, entry := range entries {
		if entry.Sequence <= ack {
			continue
		}
		raw, err := json.Marshal(entry)
		if err != nil {
			return 0, err
		}
		used += uint64(len(raw))
		if used > domain.DefaultLogLimitBytes {
			return 0, domain.ErrOperationTransition
		}
		if _, err := tx.Exec(ctx, "insert into backup_operation_logs(operation_id,sequence,entry_json) values($1,$2,$3::jsonb)", id, entry.Sequence, string(raw)); err != nil {
			return 0, err
		}
		ack = entry.Sequence
	}
	if _, err := tx.Exec(ctx, "update backup_operation_log_heads set ack_sequence=$2,bytes_received=$3 where operation_id=$1", id, ack, used); err != nil {
		return 0, err
	}
	return ack, tx.Commit(ctx)
}

func (s *Store) OperationLogs(ctx context.Context, id uuid.UUID, after uint64, limit int) ([]domain.BackupLogEntry, error) {
	rows, err := s.pool.Query(ctx, "select entry_json from backup_operation_logs where operation_id=$1 and sequence>$2 order by sequence limit $3", id, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	entries := make([]domain.BackupLogEntry, 0)
	for rows.Next() {
		var entry domain.BackupLogEntry
		if err := rows.Scan(&entry); err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	return entries, rows.Err()
}
