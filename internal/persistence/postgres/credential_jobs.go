package postgres

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sagehou/restfleet/internal/domain"
)

const operationColumns = "id,type,status,source,storage_credential_id,secret_revision,requested_by_user_id,attempt,created_at,dispatched_at,acknowledged_at,started_at,finished_at,error_code"

func scanOperation(row rowScanner) (domain.Operation, error) {
	var o domain.Operation
	err := row.Scan(&o.ID, &o.Type, &o.Status, &o.Source, &o.StorageCredentialID, &o.SecretRevision, &o.RequestedByUserID, &o.Attempt, &o.CreatedAt, &o.DispatchedAt, &o.AcknowledgedAt, &o.StartedAt, &o.FinishedAt, &o.ErrorCode)
	if errors.Is(err, pgx.ErrNoRows) {
		return o, domain.ErrNotFound
	}
	return o, err
}

func (s *Store) Operation(ctx context.Context, id uuid.UUID) (domain.Operation, error) {
	return scanOperation(s.pool.QueryRow(ctx, "select "+operationColumns+" from operations where id=$1", id))
}

func recordOperationEvent(ctx context.Context, tx pgx.Tx, o domain.Operation, from string, now time.Time) error {
	_, err := tx.Exec(ctx, `insert into operation_events(operation_id,sequence,from_status,to_status,occurred_at)
		select $1,coalesce(max(sequence),0)+1,$2,$3,$4 from operation_events where operation_id=$1`, o.ID, from, o.Status, now)
	if err != nil {
		return err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return err
	}
	payload, err := json.Marshal(map[string]string{"status": o.Status})
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `insert into outbox_events(id,event_type,aggregate_type,aggregate_id,payload,created_at,available_at)
		values($1,'OPERATION_STATE_CHANGED','OPERATION',$2,$3,$4,$4)`, id, o.ID, payload, now)
	return err
}

// All callers hold the operation row lock. No arbitrary status setter is exposed.
func transitionOperation(ctx context.Context, tx pgx.Tx, o *domain.Operation, to string, now time.Time) error {
	if err := domain.ValidateOperationTransition(o.Status, to); err != nil {
		return err
	}
	from := o.Status
	o.Status = to
	switch to {
	case "DISPATCHED":
		o.DispatchedAt = &now
	case "ACKNOWLEDGED":
		o.AcknowledgedAt = &now
	case "RUNNING":
		o.StartedAt = &now
	}
	if domain.OperationTerminal(to) {
		o.FinishedAt = &now
	}
	_, err := tx.Exec(ctx, `update operations set status=$2,dispatched_at=$3,acknowledged_at=$4,
		started_at=$5,finished_at=$6,error_code=$7 where id=$1`, o.ID, to, o.DispatchedAt, o.AcknowledgedAt, o.StartedAt, o.FinishedAt, o.ErrorCode)
	if err != nil {
		return err
	}
	return recordOperationEvent(ctx, tx, *o, from, now)
}

func (s *Store) EnqueueCredentialTest(ctx context.Context, o domain.Operation, scope, key, request []byte, audit domain.AuditEvent) (domain.Operation, error) {
	if len(scope) != 32 || len(key) != 32 || len(request) != 32 {
		return domain.Operation{}, domain.ErrIdempotencyReused
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return o, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// Same actor/route/key is serialized before reading or creating its resource.
	if _, err = tx.Exec(ctx, "select pg_advisory_xact_lock($1)", int64(binary.BigEndian.Uint64(key[:8]))); err != nil {
		return o, err
	}
	var original uuid.UUID
	var bodyHash []byte
	err = tx.QueryRow(ctx, `select resource_id,request_hash from idempotency_records
		where scope_hash=$1 and key_hash=$2 and expires_at>clock_timestamp()`, scope, key).Scan(&original, &bodyHash)
	if err == nil {
		if !bytes.Equal(bodyHash, request) {
			return o, domain.ErrIdempotencyReused
		}
		result, err := scanOperation(tx.QueryRow(ctx, "select "+operationColumns+" from operations where id=$1", original))
		if err != nil {
			return o, err
		}
		return result, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return o, err
	}
	c, err := scanCredential(tx.QueryRow(ctx, "select "+credentialColumns+" from storage_credentials where id=$1 for update", o.StorageCredentialID))
	if err != nil {
		return o, err
	}
	if c.Status == "DISABLED" {
		return o, domain.ErrCredentialDisabled
	}
	var active bool
	if err = tx.QueryRow(ctx, "select exists(select 1 from operations where storage_credential_id=$1 and finished_at is null)", c.ID).Scan(&active); err != nil {
		return o, err
	}
	if active {
		return o, domain.ErrCredentialTestBusy
	}
	// The database clock is authoritative for availability and leases.
	if err = tx.QueryRow(ctx, "select clock_timestamp()").Scan(&o.CreatedAt); err != nil {
		return o, err
	}
	o.Type, o.Status, o.Source, o.Attempt = "CREDENTIAL_TEST", "QUEUED", "USER", 1
	o.SecretRevision = c.SecretRevision
	_, err = tx.Exec(ctx, `insert into operations(id,type,status,source,storage_credential_id,secret_revision,requested_by_user_id,attempt,created_at)
		values($1,$2,$3,$4,$5,$6,$7,1,$8)`, o.ID, o.Type, o.Status, o.Source, c.ID, o.SecretRevision, o.RequestedByUserID, o.CreatedAt)
	if err != nil {
		return o, err
	}
	jobID, err := uuid.NewV7()
	if err != nil {
		return o, err
	}
	_, err = tx.Exec(ctx, `insert into jobs(id,operation_id,queue,status,available_at,created_at,updated_at)
		values($1,$2,'CREDENTIAL_TEST','READY',$3,$3,$3)`, jobID, o.ID, o.CreatedAt)
	if err != nil {
		return o, err
	}
	_, err = tx.Exec(ctx, `insert into idempotency_records(scope_hash,key_hash,request_hash,status,resource_type,resource_id,created_at,expires_at)
		values($1,$2,$3,202,'OPERATION',$4,$5,$5+interval '24 hours')
		on conflict(scope_hash,key_hash) do update set request_hash=excluded.request_hash,
		resource_id=excluded.resource_id,created_at=excluded.created_at,expires_at=excluded.expires_at`, scope, key, request, o.ID, o.CreatedAt)
	if err != nil {
		return o, err
	}
	_, err = tx.Exec(ctx, "update storage_credentials set last_test_operation_id=$2,revision=revision+1,updated_at=$3 where id=$1", c.ID, o.ID, o.CreatedAt)
	if err != nil {
		return o, err
	}
	if err = recordOperationEvent(ctx, tx, o, "", o.CreatedAt); err != nil {
		return o, err
	}
	if err = appendAudit(ctx, tx, audit); err != nil {
		return o, err
	}
	return o, tx.Commit(ctx)
}

func (s *Store) ClaimCredentialJob(ctx context.Context, owner uuid.UUID) (domain.CredentialJob, error) {
	var job domain.CredentialJob
	if owner == uuid.Nil {
		return job, domain.ErrJobLeaseLost
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return job, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var operationID uuid.UUID
	var attempt, maxAttempts int
	err = tx.QueryRow(ctx, `select id,operation_id,attempt,max_attempts from jobs
		where queue='CREDENTIAL_TEST' and ((status='READY' and available_at<=clock_timestamp())
		or (status='LEASED' and lease_expires_at<=clock_timestamp()))
		order by available_at,id for update skip locked limit 1`).Scan(&job.ID, &operationID, &attempt, &maxAttempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return job, domain.ErrNotFound
	}
	if err != nil {
		return job, err
	}
	job.Operation, err = scanOperation(tx.QueryRow(ctx, "select "+operationColumns+" from operations where id=$1 for update", operationID))
	if err != nil {
		return job, err
	}
	job.Credential, err = scanCredential(tx.QueryRow(ctx, "select "+credentialColumns+" from storage_credentials where id=$1 for update", job.Operation.StorageCredentialID))
	if err != nil {
		return job, err
	}
	var now time.Time
	if err = tx.QueryRow(ctx, "select clock_timestamp()").Scan(&now); err != nil {
		return job, err
	}
	if attempt >= maxAttempts {
		job.Operation.ErrorCode = "WORKER_LOST"
		if err = finishCredentialJob(ctx, tx, &job, "TIMED_OUT", now); err != nil {
			return job, err
		}
		if err = tx.Commit(ctx); err != nil {
			return job, err
		}
		return domain.CredentialJob{}, domain.ErrNotFound
	}
	job.Owner = owner
	job.Operation.Attempt = attempt + 1
	job.LeaseExpiresAt = now.Add(30 * time.Second)
	_, err = tx.Exec(ctx, `update jobs set status='LEASED',lease_owner=$2,lease_expires_at=$3,attempt=attempt+1,updated_at=$4 where id=$1`, job.ID, owner, job.LeaseExpiresAt, now)
	if err != nil {
		return job, err
	}
	if _, err = tx.Exec(ctx, "update operations set attempt=$2 where id=$1", operationID, job.Operation.Attempt); err != nil {
		return job, err
	}
	if job.Operation.Status == "QUEUED" {
		for _, to := range []string{"DISPATCHED", "ACKNOWLEDGED", "RUNNING"} {
			if err = transitionOperation(ctx, tx, &job.Operation, to, now); err != nil {
				return job, err
			}
		}
	} else if job.Operation.Status != "RUNNING" {
		return job, domain.ErrOperationTransition
	}
	// Every plaintext read is authorized and audited after claim, including recovery.
	audit, err := credentialJobAudit(job.Operation, "STORAGE_SECRET_ACCESS", "TEST_RUNTIME", now)
	if err != nil {
		return job, err
	}
	if err = appendAudit(ctx, tx, audit); err != nil {
		return job, err
	}
	if err = ensureCredentialLease(ctx, tx, job.ID, owner); err != nil {
		return job, err
	}
	return job, tx.Commit(ctx)
}

// Lock order is always job -> operation -> credential -> audit chain.
func lockCredentialJob(ctx context.Context, tx pgx.Tx, id, owner uuid.UUID) (domain.CredentialJob, error) {
	job := domain.CredentialJob{ID: id, Owner: owner}
	var operationID uuid.UUID
	err := tx.QueryRow(ctx, `select operation_id,lease_expires_at from jobs where id=$1 and
		status='LEASED' and lease_owner=$2 and lease_expires_at>clock_timestamp() for update`, id, owner).Scan(&operationID, &job.LeaseExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return job, domain.ErrJobLeaseLost
	}
	if err != nil {
		return job, err
	}
	job.Operation, err = scanOperation(tx.QueryRow(ctx, "select "+operationColumns+" from operations where id=$1 for update", operationID))
	if err != nil {
		return job, err
	}
	job.Credential, err = scanCredential(tx.QueryRow(ctx, "select "+credentialColumns+" from storage_credentials where id=$1 for update", job.Operation.StorageCredentialID))
	return job, err
}

func (s *Store) RenewCredentialJob(ctx context.Context, id, owner uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, `update jobs set lease_expires_at=clock_timestamp()+interval '30 seconds',updated_at=clock_timestamp()
		where id=$1 and status='LEASED' and lease_owner=$2 and lease_expires_at>clock_timestamp()`, id, owner)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return domain.ErrJobLeaseLost
	}
	return nil
}

func credentialUnchanged(job domain.CredentialJob) bool {
	return job.Credential.Status != "DISABLED" && job.Credential.SecretRevision == job.Operation.SecretRevision
}

func (s *Store) RefreshCredentialJob(ctx context.Context, id, owner uuid.UUID, expected int64, e domain.SecretEnvelope) (domain.StorageCredential, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return domain.StorageCredential{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	job, err := lockCredentialJob(ctx, tx, id, owner)
	if err != nil {
		return job.Credential, err
	}
	if !credentialUnchanged(job) || job.Operation.SecretRevision != expected {
		return job.Credential, domain.ErrRevisionConflict
	}
	var now time.Time
	if err = tx.QueryRow(ctx, "select clock_timestamp()").Scan(&now); err != nil {
		return job.Credential, err
	}
	if err = insertStorageSecret(ctx, tx, e); err != nil {
		return job.Credential, err
	}
	_, err = tx.Exec(ctx, `insert into storage_credential_revisions(credential_id,revision,secret_ref,created_at)
		values($1,$2,$3,$4)`, job.Credential.ID, expected+1, e.ID, now)
	if err != nil {
		return job.Credential, err
	}
	c, err := scanCredential(tx.QueryRow(ctx, `update storage_credentials set secret_ref=$2,secret_revision=secret_revision+1,
		revision=revision+1,last_refreshed_at=$3,updated_at=$3 where id=$1 returning `+credentialColumns, job.Credential.ID, e.ID, now))
	if err != nil {
		return c, err
	}
	if _, err = tx.Exec(ctx, "update operations set secret_revision=$2 where id=$1", job.Operation.ID, c.SecretRevision); err != nil {
		return c, err
	}
	audit, err := credentialJobAudit(job.Operation, "STORAGE_CREDENTIAL_REFRESH", "SECRET_CHANGED", now)
	if err != nil {
		return c, err
	}
	audit.Changes, err = json.Marshal(map[string]any{"secret": map[string]any{"changed": true, "revision": c.SecretRevision}})
	if err != nil {
		return c, err
	}
	if err = appendAudit(ctx, tx, audit); err != nil {
		return c, err
	}
	// Recheck time after writes: an expired owner must not commit even if it
	// held locks while audit contention consumed the rest of its lease.
	if err = ensureCredentialLease(ctx, tx, id, owner); err != nil {
		return c, err
	}
	return c, tx.Commit(ctx)
}

func ensureCredentialLease(ctx context.Context, tx pgx.Tx, id, owner uuid.UUID) error {
	var valid bool
	err := tx.QueryRow(ctx, "select exists(select 1 from jobs where id=$1 and status='LEASED' and lease_owner=$2 and lease_expires_at>clock_timestamp())", id, owner).Scan(&valid)
	if err != nil {
		return err
	}
	if !valid {
		return domain.ErrJobLeaseLost
	}
	return nil
}

func (s *Store) CompleteCredentialJob(ctx context.Context, id, owner uuid.UUID, code string) error {
	if !domain.ValidCredentialTestCode(code) {
		return domain.ErrOperationTransition
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	job, err := lockCredentialJob(ctx, tx, id, owner)
	if err != nil {
		return err
	}
	var now time.Time
	if err = tx.QueryRow(ctx, "select clock_timestamp()").Scan(&now); err != nil {
		return err
	}
	job.Operation.ErrorCode = code
	if !credentialUnchanged(job) {
		job.Operation.ErrorCode = "CREDENTIAL_CHANGED"
		if job.Credential.Status == "DISABLED" {
			job.Operation.ErrorCode = "CREDENTIAL_DISABLED"
		}
	}
	to := "FAILED"
	if job.Operation.ErrorCode == "" {
		to = "SUCCEEDED"
	} else if job.Operation.ErrorCode == "TEST_TIMED_OUT" {
		to = "TIMED_OUT"
	}
	if err = ensureCredentialLease(ctx, tx, id, owner); err != nil {
		return err
	}
	if err = finishCredentialJob(ctx, tx, &job, to, now); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func finishCredentialJob(ctx context.Context, tx pgx.Tx, job *domain.CredentialJob, to string, now time.Time) error {
	if err := transitionOperation(ctx, tx, &job.Operation, to, now); err != nil {
		return err
	}
	if credentialUnchanged(*job) {
		health := "DEGRADED"
		if to == "SUCCEEDED" {
			health = "HEALTHY"
		}
		_, err := tx.Exec(ctx, `update storage_credentials set status=$2,last_tested_at=$3,last_test_result=case when $4='' then 'SUCCEEDED' else $4 end,
			revision=revision+1,updated_at=$3 where id=$1`, job.Credential.ID, health, now, job.Operation.ErrorCode)
		if err != nil {
			return err
		}
	}
	audit, err := credentialJobAudit(job.Operation, "STORAGE_CREDENTIAL_TEST_RESULT", job.Operation.Status, now)
	if err != nil {
		return err
	}
	if to != "SUCCEEDED" {
		audit.Result = domain.AuditFailure
	}
	if err = appendAudit(ctx, tx, audit); err != nil {
		return err
	}
	// Completion keeps the lease fence in the final UPDATE, after audit writes.
	status := "DONE"
	if to != "SUCCEEDED" {
		status = "DEAD"
	}
	query := `update jobs set status=$2,lease_owner=null,lease_expires_at=null,last_error_code=$3,updated_at=$4 where id=$1`
	args := []any{job.ID, status, job.Operation.ErrorCode, now}
	if job.Owner != uuid.Nil {
		query += " and lease_owner=$5 and lease_expires_at>clock_timestamp()"
		args = append(args, job.Owner)
	}
	tag, err := tx.Exec(ctx, query, args...)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return domain.ErrJobLeaseLost
	}
	return nil
}

func credentialJobAudit(o domain.Operation, action, reason string, now time.Time) (domain.AuditEvent, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return domain.AuditEvent{}, err
	}
	return domain.AuditEvent{ID: id, OccurredAt: now, ActorType: domain.ActorSystem,
		Action: action, ResourceType: "STORAGE_CREDENTIAL", ResourceID: o.StorageCredentialID,
		RequestID: o.ID, Result: domain.AuditSuccess, ReasonCode: reason}, nil
}
