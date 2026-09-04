package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sagehou/restfleet/internal/domain"
)

type rowScanner interface {
	Scan(dest ...any) error
}

func (s *Store) CreateHost(ctx context.Context, host domain.Host, audit domain.AuditEvent) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	labels, err := json.Marshal(host.Labels)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
		insert into hosts (
			id, display_name, description, labels, timezone, status,
			revision, created_at, updated_at
		) values ($1, $2, $3, $4, $5, $6, $7, $8, $8)
	`, host.ID, host.DisplayName, host.Description, labels, host.Timezone,
		host.Status, host.Revision, host.CreatedAt)
	if err != nil {
		return persistenceError(err)
	}
	if err := appendAudit(ctx, tx, audit); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) Hosts(ctx context.Context) ([]domain.Host, error) {
	rows, err := s.pool.Query(ctx, `
		select id, display_name, description, labels, timezone, status,
		       revision, created_at, updated_at
		from hosts
		where archived_at is null
		order by lower(display_name), id
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	hosts := make([]domain.Host, 0)
	for rows.Next() {
		host, err := scanHost(rows)
		if err != nil {
			return nil, err
		}
		hosts = append(hosts, host)
	}
	return hosts, rows.Err()
}

func (s *Store) Host(ctx context.Context, id uuid.UUID) (domain.Host, error) {
	host, err := scanHost(s.pool.QueryRow(ctx, `
		select id, display_name, description, labels, timezone, status,
		       revision, created_at, updated_at
		from hosts
		where id = $1 and archived_at is null
	`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Host{}, domain.ErrNotFound
	}
	return host, err
}

func scanHost(row rowScanner) (domain.Host, error) {
	var host domain.Host
	var labels []byte
	err := row.Scan(
		&host.ID, &host.DisplayName, &host.Description, &labels, &host.Timezone,
		&host.Status, &host.Revision, &host.CreatedAt, &host.UpdatedAt,
	)
	if err != nil {
		return domain.Host{}, err
	}
	if err := json.Unmarshal(labels, &host.Labels); err != nil {
		return domain.Host{}, err
	}
	return host, nil
}

func (s *Store) UpdateHost(
	ctx context.Context,
	host domain.Host,
	expectedRevision int64,
	audit domain.AuditEvent,
) (domain.Host, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return domain.Host{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	labels, err := json.Marshal(host.Labels)
	if err != nil {
		return domain.Host{}, err
	}
	updated, err := scanHost(tx.QueryRow(ctx, `
		update hosts
		set display_name = $3, description = $4, labels = $5, timezone = $6,
		    revision = revision + 1, updated_at = $7
		where id = $1 and revision = $2 and archived_at is null
		returning id, display_name, description, labels, timezone, status,
		          revision, created_at, updated_at
	`, host.ID, expectedRevision, host.DisplayName, host.Description, labels,
		host.Timezone, host.UpdatedAt))
	if errors.Is(err, pgx.ErrNoRows) {
		if hostExists(ctx, tx, host.ID) {
			return domain.Host{}, domain.ErrRevisionConflict
		}
		return domain.Host{}, domain.ErrNotFound
	}
	if err != nil {
		return domain.Host{}, persistenceError(err)
	}
	if err := appendAudit(ctx, tx, audit); err != nil {
		return domain.Host{}, err
	}
	return updated, tx.Commit(ctx)
}

func (s *Store) SetHostStatus(
	ctx context.Context,
	id uuid.UUID,
	expectedRevision int64,
	status string,
	at time.Time,
	audit domain.AuditEvent,
) (domain.Host, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return domain.Host{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	updated, err := scanHost(tx.QueryRow(ctx, `
		update hosts
		set status = case
		      when $3 = 'ENABLE' and exists (
		        select 1 from agents where host_id = hosts.id and status = 'ACTIVE'
		      ) then 'ACTIVE'
		      when $3 = 'ENABLE' then 'PENDING'
		      else 'DISABLED'
		    end,
		    revision = revision + 1,
		    updated_at = $4
		where id = $1 and revision = $2 and status <> 'REVOKED' and archived_at is null
		returning id, display_name, description, labels, timezone, status,
		          revision, created_at, updated_at
	`, id, expectedRevision, status, at))
	if errors.Is(err, pgx.ErrNoRows) {
		if hostExists(ctx, tx, id) {
			return domain.Host{}, domain.ErrRevisionConflict
		}
		return domain.Host{}, domain.ErrNotFound
	}
	if err != nil {
		return domain.Host{}, err
	}
	if err := appendAudit(ctx, tx, audit); err != nil {
		return domain.Host{}, err
	}
	return updated, tx.Commit(ctx)
}

func hostExists(ctx context.Context, tx pgx.Tx, id uuid.UUID) bool {
	var exists bool
	if err := tx.QueryRow(ctx, "select exists(select 1 from hosts where id = $1 and archived_at is null)", id).Scan(&exists); err != nil {
		return false
	}
	return exists
}

func (s *Store) CreateEnrollmentToken(
	ctx context.Context,
	token domain.EnrollmentToken,
	audit domain.AuditEvent,
) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, `
		insert into enrollment_tokens (
			id, host_id, token_hash, token_fingerprint, expires_at, created_by, created_at
		)
		select $1, id, $3, $4, $5, $6, $7
		from hosts
		where id = $2 and status in ('PENDING', 'ACTIVE') and archived_at is null
	`, token.ID, token.HostID, token.TokenHash, token.Fingerprint,
		token.ExpiresAt, token.CreatedBy, token.CreatedAt)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return domain.ErrNotFound
	}
	if err := appendAudit(ctx, tx, audit); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) EnrollmentTokens(ctx context.Context, hostID uuid.UUID) ([]domain.EnrollmentToken, error) {
	rows, err := s.pool.Query(ctx, `
		select id, host_id, token_hash, token_fingerprint, expires_at, created_by,
		       created_at, used_at, used_by_agent_id, revoked_at
		from enrollment_tokens
		where host_id = $1
		order by created_at desc
	`, hostID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tokens := make([]domain.EnrollmentToken, 0)
	for rows.Next() {
		var token domain.EnrollmentToken
		if err := rows.Scan(
			&token.ID, &token.HostID, &token.TokenHash, &token.Fingerprint,
			&token.ExpiresAt, &token.CreatedBy, &token.CreatedAt, &token.UsedAt,
			&token.UsedByAgentID, &token.RevokedAt,
		); err != nil {
			return nil, err
		}
		tokens = append(tokens, token)
	}
	return tokens, rows.Err()
}

func (s *Store) RevokeEnrollmentToken(
	ctx context.Context,
	id uuid.UUID,
	at time.Time,
	audit domain.AuditEvent,
) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, `
		update enrollment_tokens
		set revoked_at = $2
		where id = $1 and used_at is null and revoked_at is null and expires_at > $2
	`, id, at)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return domain.ErrNotFound
	}
	if err := appendAudit(ctx, tx, audit); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) ConsumeEnrollmentToken(
	ctx context.Context,
	tokenHash []byte,
	now time.Time,
	issue domain.EnrollmentIssuer,
) (domain.EnrollmentMaterial, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return domain.EnrollmentMaterial{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var hostID uuid.UUID
	var usedAt, revokedAt *time.Time
	var expiresAt time.Time
	var hostStatus string
	err = tx.QueryRow(ctx, `
		select t.host_id, t.expires_at, t.used_at, t.revoked_at, h.status
		from enrollment_tokens t
		join hosts h on h.id = t.host_id
		where t.token_hash = $1
		for update of t
	`, tokenHash).Scan(&hostID, &expiresAt, &usedAt, &revokedAt, &hostStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.EnrollmentMaterial{}, domain.ErrEnrollmentTokenInvalid
	}
	if err != nil {
		return domain.EnrollmentMaterial{}, err
	}
	if usedAt != nil || revokedAt != nil || !now.Before(expiresAt) ||
		(hostStatus != domain.HostPending && hostStatus != domain.HostActive) {
		return domain.EnrollmentMaterial{}, domain.ErrEnrollmentTokenInvalid
	}
	material, err := issue(hostID)
	if err != nil {
		return domain.EnrollmentMaterial{}, err
	}
	agent := material.Agent
	certificate := material.Certificate
	_, err = tx.Exec(ctx, `
		insert into agents (
			id, host_id, install_id, public_key_fingerprint, certificate_serial,
			certificate_not_after, status, version, protocol_version, os, arch,
			hostname, created_at, updated_at
		) values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $13)
	`, agent.ID, hostID, agent.InstallID, agent.PublicKeyFingerprint,
		agent.CertificateSerial, agent.CertificateNotAfter, agent.Status,
		agent.Version, agent.ProtocolVersion, agent.OS, agent.Arch,
		agent.Hostname, agent.CreatedAt)
	if err != nil {
		return domain.EnrollmentMaterial{}, persistenceError(err)
	}
	_, err = tx.Exec(ctx, `
		insert into agent_certificates (
			id, agent_id, serial_number, public_key_fingerprint, not_before,
			not_after, issued_at
		) values ($1, $2, $3, $4, $5, $6, $7)
	`, certificate.ID, agent.ID, certificate.SerialNumber,
		certificate.PublicKeyFingerprint, certificate.NotBefore,
		certificate.NotAfter, certificate.IssuedAt)
	if err != nil {
		return domain.EnrollmentMaterial{}, err
	}
	tag, err := tx.Exec(ctx, `
		update enrollment_tokens
		set used_at = $2, used_by_agent_id = $3
		where token_hash = $1 and used_at is null and revoked_at is null and expires_at > $2
	`, tokenHash, now, agent.ID)
	if err != nil {
		return domain.EnrollmentMaterial{}, err
	}
	if tag.RowsAffected() != 1 {
		return domain.EnrollmentMaterial{}, domain.ErrEnrollmentTokenInvalid
	}
	_, err = tx.Exec(ctx, `
		update hosts
		set status = 'ACTIVE', revision = revision + 1, updated_at = $2
		where id = $1 and status in ('PENDING', 'ACTIVE')
	`, hostID, now)
	if err != nil {
		return domain.EnrollmentMaterial{}, err
	}
	if err := appendAudit(ctx, tx, material.Audit); err != nil {
		return domain.EnrollmentMaterial{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.EnrollmentMaterial{}, err
	}
	return material, nil
}

func (s *Store) AgentsForHost(ctx context.Context, hostID uuid.UUID) ([]domain.Agent, error) {
	rows, err := s.pool.Query(ctx, agentSelect+`
		where host_id = $1
		order by created_at desc
	`, hostID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	agents := make([]domain.Agent, 0)
	for rows.Next() {
		agent, err := scanAgent(rows)
		if err != nil {
			return nil, err
		}
		agents = append(agents, agent)
	}
	return agents, rows.Err()
}

func (s *Store) Agent(ctx context.Context, id uuid.UUID) (domain.Agent, error) {
	agent, err := scanAgent(s.pool.QueryRow(ctx, agentSelect+" where id = $1", id))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Agent{}, domain.ErrNotFound
	}
	return agent, err
}

func (s *Store) AgentByCertificate(
	ctx context.Context,
	id uuid.UUID,
	serial string,
	now time.Time,
) (domain.Agent, error) {
	agent, err := scanAgent(s.pool.QueryRow(ctx, agentSelect+`
		join agent_certificates c on c.agent_id = agents.id
		where agents.id = $1 and c.serial_number = $2
		  and agents.status = 'ACTIVE'
		  and c.revoked_at is null
		  and c.not_before <= $3 and c.not_after > $3
		  and (c.superseded_by is null or c.overlap_ends_at > $3)
	`, id, serial, now))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Agent{}, domain.ErrAgentRevoked
	}
	return agent, err
}

const agentSelect = `
	select agents.id, agents.host_id, agents.install_id, agents.public_key_fingerprint,
	       agents.certificate_serial, agents.certificate_not_after, agents.status,
	       agents.version, agents.protocol_version, agents.os, agents.arch,
	       agents.hostname, agents.boot_id, agents.restic_version,
	       agents.last_seen_at, agents.last_connected_at, agents.desired_revision,
	       agents.accepted_revision, agents.created_at, agents.updated_at
	from agents
`

func scanAgent(row rowScanner) (domain.Agent, error) {
	var agent domain.Agent
	err := row.Scan(
		&agent.ID, &agent.HostID, &agent.InstallID, &agent.PublicKeyFingerprint,
		&agent.CertificateSerial, &agent.CertificateNotAfter, &agent.Status,
		&agent.Version, &agent.ProtocolVersion, &agent.OS, &agent.Arch,
		&agent.Hostname, &agent.BootID, &agent.ResticVersion, &agent.LastSeenAt,
		&agent.LastConnectedAt, &agent.DesiredRevision, &agent.AcceptedRevision,
		&agent.CreatedAt, &agent.UpdatedAt,
	)
	return agent, err
}

func (s *Store) MarkAgentConnected(
	ctx context.Context,
	id, installID uuid.UUID,
	version, protocolVersion, hostname, bootID, resticVersion string,
	at time.Time,
) error {
	tag, err := s.pool.Exec(ctx, `
		update agents
		set version = $3, protocol_version = $4, hostname = $5, boot_id = $6,
		    restic_version = $7, last_seen_at = $8, last_connected_at = $8,
		    updated_at = $8
		where id = $1 and install_id = $2 and status = 'ACTIVE'
	`, id, installID, version, protocolVersion, hostname, bootID, resticVersion, at)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return domain.ErrAgentRevoked
	}
	return nil
}

func (s *Store) RevokeAgent(
	ctx context.Context,
	id uuid.UUID,
	reason string,
	at time.Time,
	audit domain.AuditEvent,
) (domain.Agent, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return domain.Agent{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	agent, err := scanAgent(tx.QueryRow(ctx, agentSelect+" where id = $1 for update", id))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Agent{}, domain.ErrNotFound
	}
	if err != nil {
		return domain.Agent{}, err
	}
	if agent.Status == domain.AgentRevoked {
		return agent, tx.Commit(ctx)
	}
	tag, err := tx.Exec(ctx, `
		update agents set status = 'REVOKED', updated_at = $2
		where id = $1 and status = 'ACTIVE'
	`, id, at)
	if err != nil {
		return domain.Agent{}, err
	}
	if tag.RowsAffected() != 1 {
		return domain.Agent{}, domain.ErrNotFound
	}
	_, err = tx.Exec(ctx, `
		update agent_certificates
		set revoked_at = $2, revocation_reason = $3
		where agent_id = $1 and revoked_at is null
	`, id, at, reason)
	if err != nil {
		return domain.Agent{}, err
	}
	_, err = tx.Exec(ctx, `
		update hosts set status = 'PENDING', revision = revision + 1, updated_at = $2
		where id = (select host_id from agents where id = $1) and status = 'ACTIVE'
	`, id, at)
	if err != nil {
		return domain.Agent{}, err
	}
	if err := appendAudit(ctx, tx, audit); err != nil {
		return domain.Agent{}, err
	}
	agent, err = scanAgent(tx.QueryRow(ctx, agentSelect+" where id = $1", id))
	if err != nil {
		return domain.Agent{}, err
	}
	return agent, tx.Commit(ctx)
}

func (s *Store) RotateAgentCertificate(
	ctx context.Context,
	agentID uuid.UUID,
	currentSerial string,
	certificate domain.AgentCertificate,
	at time.Time,
	overlapEndsAt time.Time,
	audit domain.AuditEvent,
) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var status string
	err = tx.QueryRow(ctx, `
		select a.status
		from agents a
		join agent_certificates c on c.agent_id = a.id
		where a.id = $1 and c.serial_number = $2 and c.revoked_at is null
		  and c.not_after > $3
		for update of a, c
	`, agentID, currentSerial, at).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) || status != domain.AgentActive {
		return domain.ErrAgentRevoked
	}
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
		insert into agent_certificates (
			id, agent_id, serial_number, public_key_fingerprint,
			not_before, not_after, issued_at
		) values ($1, $2, $3, $4, $5, $6, $7)
	`, certificate.ID, agentID, certificate.SerialNumber,
		certificate.PublicKeyFingerprint, certificate.NotBefore,
		certificate.NotAfter, certificate.IssuedAt)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
		update agent_certificates
		set superseded_by = $3, overlap_ends_at = $4
		where agent_id = $1 and serial_number = $2
	`, agentID, currentSerial, certificate.ID, overlapEndsAt)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
		update agents
		set public_key_fingerprint = $2, certificate_serial = $3,
		    certificate_not_after = $4, updated_at = $5
		where id = $1
	`, agentID, certificate.PublicKeyFingerprint, certificate.SerialNumber,
		certificate.NotAfter, at)
	if err != nil {
		return err
	}
	if err := appendAudit(ctx, tx, audit); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) AgentCA(ctx context.Context) (domain.AgentCARecord, error) {
	var record domain.AgentCARecord
	envelope := &record.PrivateKey
	err := s.pool.QueryRow(ctx, `
		select p.ca_certificate_pem, p.created_at,
		       s.id, s.kind, s.algorithm, s.key_id, s.ciphertext, s.nonce,
		       s.wrapped_data_key, s.wrap_nonce, s.aad, s.created_at
		from server_pki p
		join secrets s on s.id = p.private_key_secret_id
		where p.singleton = true
	`).Scan(
		&record.CertificatePEM, &record.CreatedAt,
		&envelope.ID, &envelope.Kind, &envelope.Algorithm, &envelope.KeyID,
		&envelope.Ciphertext, &envelope.Nonce, &envelope.WrappedDataKey,
		&envelope.WrapNonce, &envelope.AAD, &envelope.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.AgentCARecord{}, domain.ErrNotFound
	}
	return record, err
}

func (s *Store) InitializeAgentCA(
	ctx context.Context,
	record domain.AgentCARecord,
) (domain.AgentCARecord, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return domain.AgentCARecord{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "select pg_advisory_xact_lock($1)", int64(6740481908302)); err != nil {
		return domain.AgentCARecord{}, err
	}
	var exists bool
	if err := tx.QueryRow(ctx, "select exists(select 1 from server_pki where singleton = true)").Scan(&exists); err != nil {
		return domain.AgentCARecord{}, err
	}
	if exists {
		if err := tx.Commit(ctx); err != nil {
			return domain.AgentCARecord{}, err
		}
		return s.AgentCA(ctx)
	}
	envelope := record.PrivateKey
	_, err = tx.Exec(ctx, `
		insert into secrets (
			id, kind, algorithm, key_id, ciphertext, nonce, wrapped_data_key,
			wrap_nonce, aad, created_at
		) values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
	`, envelope.ID, envelope.Kind, envelope.Algorithm, envelope.KeyID,
		envelope.Ciphertext, envelope.Nonce, envelope.WrappedDataKey,
		envelope.WrapNonce, envelope.AAD, envelope.CreatedAt)
	if err != nil {
		return domain.AgentCARecord{}, err
	}
	_, err = tx.Exec(ctx, `
		insert into server_pki (
			singleton, ca_certificate_pem, private_key_secret_id, created_at
		) values (true, $1, $2, $3)
	`, record.CertificatePEM, envelope.ID, record.CreatedAt)
	if err != nil {
		return domain.AgentCARecord{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.AgentCARecord{}, err
	}
	return record, nil
}

func persistenceError(err error) error {
	var databaseError *pgconn.PgError
	if errors.As(err, &databaseError) && databaseError.Code == "23505" {
		return domain.ErrAlreadyExists
	}
	return err
}
