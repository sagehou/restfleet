package postgres

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/sagehou/restfleet/internal/domain"
)

// Agent -> Host -> Repository -> credential -> delivery -> audit. Agent first
// matches revocation; Host locking also excludes concurrent disable/archive.
func lockAgentRepository(ctx context.Context, tx pgx.Tx, agentID uuid.UUID, credentialWrite bool) (domain.Repository, error) {
	var hostID uuid.UUID
	err := tx.QueryRow(ctx, "select host_id from agents where id=$1 and status='ACTIVE' for update", agentID).Scan(&hostID)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Repository{}, domain.ErrNotFound
	}
	if err != nil {
		return domain.Repository{}, err
	}
	var active bool
	err = tx.QueryRow(ctx, "select status='ACTIVE' and archived_at is null from hosts where id=$1 for share", hostID).Scan(&active)
	if err != nil {
		return domain.Repository{}, err
	}
	if !active {
		return domain.Repository{}, domain.ErrNotFound
	}
	r, err := scanRepository(tx.QueryRow(ctx, "select "+repositoryColumns+" from repositories where host_id=$1 and archived_at is null for update", hostID))
	if err != nil {
		return r, err
	}
	if r.InitializedAt == nil || (r.Status != "PROVISIONING" && r.Status != "READY" && r.Status != "DEGRADED" && r.Status != "LOCKED") {
		return r, domain.ErrNotFound
	}
	credentialLock := "for share"
	if credentialWrite {
		credentialLock = "for update"
	}
	err = tx.QueryRow(ctx, "select status<>'DISABLED' from storage_credentials where id=$1 "+credentialLock, r.StorageCredentialID).Scan(&active)
	if err != nil {
		return r, err
	}
	if !active {
		return r, domain.ErrNotFound
	}
	return r, nil
}

func scanAgentDelivery(ctx context.Context, tx pgx.Tx, agentID uuid.UUID) (domain.AgentCredentialDelivery, error) {
	var d domain.AgentCredentialDelivery
	err := tx.QueryRow(ctx, `select id,agent_id,revision,configuration_hash,repository_id,gateway_secret_ref,restic_secret_ref,created_at,accepted_at
		from repository_agent_deliveries where agent_id=$1 for update`, agentID).Scan(&d.ID, &d.AgentID, &d.Revision, &d.ConfigurationHash,
		&d.Repository.ID, &d.Gateway.ID, &d.Restic.ID, &d.CreatedAt, &d.AcceptedAt)
	return d, err
}

func agentCredentialAudit(ctx context.Context, tx pgx.Tx, d domain.AgentCredentialDelivery, action string) error {
	id, err := uuid.NewV7()
	if err != nil {
		return err
	}
	changes, err := json.Marshal(map[string]int64{"revision": d.Revision})
	if err != nil {
		return err
	}
	return appendAudit(ctx, tx, domain.AuditEvent{ID: id, OccurredAt: time.Now().UTC(), ActorType: domain.ActorAgent, ActorID: d.AgentID,
		Action: action, ResourceType: "REPOSITORY", ResourceID: d.Repository.ID, RequestID: d.ID, Result: domain.AuditSuccess, ReasonCode: "CREDENTIAL_REVISION", Changes: changes})
}

func readDeliverySecret(ctx context.Context, tx pgx.Tx, r domain.Repository, kind string, ref uuid.UUID, revision int64) (domain.SecretEnvelope, error) {
	var e domain.SecretEnvelope
	err := tx.QueryRow(ctx, `select s.id,s.kind,s.algorithm,s.key_id,s.ciphertext,s.nonce,s.wrapped_data_key,s.wrap_nonce,s.aad,s.created_at
		from repository_credential_revisions v join secrets s on s.id=v.secret_ref
		where v.repository_id=$1 and v.kind=$2 and v.secret_ref=$3 and v.revision=$4 and v.retired_at is null`, r.ID, kind, ref, revision).
		Scan(&e.ID, &e.Kind, &e.Algorithm, &e.KeyID, &e.Ciphertext, &e.Nonce, &e.WrappedDataKey, &e.WrapNonce, &e.AAD, &e.CreatedAt)
	return e, err
}

// No caller-supplied Host, Repository or secret ID can select the material.
// The persistent current record is authoritative; no in-memory dispatch queue.
func (s *Store) PrepareAgentCredential(ctx context.Context, agentID uuid.UUID, configurationHash string, force bool) (domain.AgentCredentialDelivery, error) {
	decoded, err := hex.DecodeString(configurationHash)
	if err != nil || len(decoded) != 32 {
		return domain.AgentCredentialDelivery{}, domain.ErrRepositoryCredential
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return domain.AgentCredentialDelivery{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	r, err := lockAgentRepository(ctx, tx, agentID, false)
	if err != nil {
		return domain.AgentCredentialDelivery{}, err
	}
	d, err := scanAgentDelivery(ctx, tx, agentID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return d, err
	}
	changed := errors.Is(err, pgx.ErrNoRows) || d.ConfigurationHash != configurationHash || d.Repository.ID != r.ID || d.Gateway.ID != r.GatewaySecretRef || d.Restic.ID != r.ResticSecretRef
	if changed {
		d.ID, err = uuid.NewV7()
		if err != nil {
			return d, err
		}
		d.Revision++
		d.AgentID, d.ConfigurationHash, d.Repository = agentID, configurationHash, r
		d.AcceptedAt = nil
		if err = tx.QueryRow(ctx, "select clock_timestamp()").Scan(&d.CreatedAt); err != nil {
			return d, err
		}
		_, err = tx.Exec(ctx, `insert into repository_agent_deliveries(agent_id,id,repository_id,revision,configuration_hash,gateway_secret_ref,restic_secret_ref,created_at)
			values($1,$2,$3,$4,$5,$6,$7,$8) on conflict(agent_id) do update set id=excluded.id,repository_id=excluded.repository_id,
			revision=excluded.revision,configuration_hash=excluded.configuration_hash,gateway_secret_ref=excluded.gateway_secret_ref,
			restic_secret_ref=excluded.restic_secret_ref,created_at=excluded.created_at,accepted_at=null`, agentID, d.ID, r.ID, d.Revision, configurationHash, r.GatewaySecretRef, r.ResticSecretRef, d.CreatedAt)
		if err != nil {
			return d, err
		}
		_, err = tx.Exec(ctx, `insert into outbox_events(id,event_type,aggregate_type,aggregate_id,payload,created_at,available_at)
			values($1,'AGENT_CREDENTIAL_CHANGED','AGENT',$2,jsonb_build_object('revision',$3::bigint),$4,$4)`, d.ID, agentID, d.Revision, d.CreatedAt)
		if err != nil {
			return d, err
		}
	}
	if !force && d.AcceptedAt != nil {
		return domain.AgentCredentialDelivery{}, domain.ErrNotFound
	}
	d.Repository = r
	d.Gateway, err = readDeliverySecret(ctx, tx, r, "GATEWAY", r.GatewaySecretRef, r.GatewaySecretRevision)
	if err != nil {
		return d, err
	}
	d.Restic, err = readDeliverySecret(ctx, tx, r, "RESTIC_KEY", r.ResticSecretRef, r.ResticSecretRevision)
	if err != nil {
		return d, err
	}
	// Audit commits before the service may decrypt either envelope or send bytes.
	if err = agentCredentialAudit(ctx, tx, d, "AGENT_REPOSITORY_SECRET_ACCESS"); err != nil {
		return d, err
	}
	return d, tx.Commit(ctx)
}

func (s *Store) AcceptAgentCredential(ctx context.Context, agentID, deliveryID uuid.UUID, revision int64, configurationHash string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	r, err := lockAgentRepository(ctx, tx, agentID, false)
	if err != nil {
		return err
	}
	d, err := scanAgentDelivery(ctx, tx, agentID)
	if err != nil {
		return err
	}
	if d.ConfigurationHash != configurationHash || d.ID != deliveryID || d.Revision != revision || d.Repository.ID != r.ID || d.Gateway.ID != r.GatewaySecretRef || d.Restic.ID != r.ResticSecretRef {
		return domain.ErrRepositoryCredential
	}
	if d.AcceptedAt != nil {
		return tx.Commit(ctx)
	}
	if _, err = tx.Exec(ctx, "update repository_agent_deliveries set accepted_at=clock_timestamp() where agent_id=$1", agentID); err != nil {
		return err
	}
	// Superseded deliveries are reconciled only by accepting the current one.
	_, err = tx.Exec(ctx, `update outbox_events set published_at=coalesce(published_at,clock_timestamp())
		where aggregate_type='AGENT' and aggregate_id=$1 and event_type='AGENT_CREDENTIAL_CHANGED'
		and (payload->>'revision')::bigint <= $2`, agentID, revision)
	if err != nil {
		return err
	}
	if err = agentCredentialAudit(ctx, tx, d, "AGENT_REPOSITORY_CREDENTIAL_ACCEPTED"); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
