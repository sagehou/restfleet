// Package postgres implements RestFleet persistence using PostgreSQL.
package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sagehou/restfleet/internal/domain"
)

const ExpectedSchemaVersion = 6

// Store is the PostgreSQL adapter for the M1 control plane.
type Store struct {
	pool *pgxpool.Pool
}

func Open(ctx context.Context, databaseURL string) (*Store, error) {
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse database configuration: %w", err)
	}
	config.ConnConfig.RuntimeParams["application_name"] = "restfleet-server"
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	return &Store{pool: pool}, nil
}

func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

func (s *Store) Close() {
	s.pool.Close()
}

func (s *Store) Ping(ctx context.Context) error {
	return s.pool.Ping(ctx)
}

func (s *Store) SchemaVersion(ctx context.Context) (int, error) {
	var version int
	err := s.pool.QueryRow(ctx, `
		select coalesce(max(version_id) filter (where is_applied), 0)
		from goose_db_version
	`).Scan(&version)
	if err != nil {
		return 0, fmt.Errorf("read schema version: %w", err)
	}
	return version, nil
}

func (s *Store) BootstrapRequired(ctx context.Context) (bool, error) {
	var required bool
	err := s.pool.QueryRow(ctx, `
		select completed_at is null and not exists (select 1 from users)
		from bootstrap_state
		where singleton = true
	`).Scan(&required)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("bootstrap state missing")
	}
	if err != nil {
		return false, fmt.Errorf("read bootstrap state: %w", err)
	}
	return required, nil
}

func (s *Store) Bootstrap(ctx context.Context, user domain.User, session domain.Session, audit domain.AuditEvent) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var completedAt *time.Time
	var hasUsers bool
	err = tx.QueryRow(ctx, `
		select completed_at, exists (select 1 from users)
		from bootstrap_state
		where singleton = true
		for update
	`).Scan(&completedAt, &hasUsers)
	if err != nil {
		return err
	}
	if completedAt != nil || hasUsers {
		audit.Result = domain.AuditDenied
		audit.ReasonCode = "BOOTSTRAP_CLOSED"
		if err := appendAudit(ctx, tx, audit); err != nil {
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
		return domain.ErrBootstrapClosed
	}

	_, err = tx.Exec(ctx, `
		insert into users (
			id, username, display_name, password_hash, role, status, created_at, updated_at
		) values ($1, $2, $3, $4, $5, $6, $7, $7)
	`, user.ID, user.Username, user.DisplayName, user.PasswordHash,
		user.Role, user.Status, user.CreatedAt)
	if err != nil {
		return err
	}
	if err := insertSession(ctx, tx, session); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
		update bootstrap_state
		set completed_at = $1, completed_by = $2
		where singleton = true
	`, user.CreatedAt, user.ID)
	if err != nil {
		return err
	}

	audit.ActorType = domain.ActorUser
	audit.ActorID = user.ID
	audit.ResourceType = "USER"
	audit.ResourceID = user.ID
	audit.Result = domain.AuditSuccess
	audit.ReasonCode = "ADMIN_CREATED"
	if err := appendAudit(ctx, tx, audit); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) FindUserByUsername(ctx context.Context, username string) (domain.User, error) {
	var user domain.User
	err := s.pool.QueryRow(ctx, `
		select id, username, display_name, password_hash, role, status,
		       last_login_at, created_at, updated_at
		from users
		where lower(username) = lower($1)
	`, username).Scan(
		&user.ID,
		&user.Username,
		&user.DisplayName,
		&user.PasswordHash,
		&user.Role,
		&user.Status,
		&user.LastLoginAt,
		&user.CreatedAt,
		&user.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.User{}, domain.ErrNotFound
	}
	if err != nil {
		return domain.User{}, err
	}
	return user, nil
}

func (s *Store) CreateLoginSession(ctx context.Context, userID uuid.UUID, session domain.Session, audit domain.AuditEvent, at time.Time) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx, `
		update users
		set last_login_at = $1, updated_at = $1
		where id = $2 and status = 'ACTIVE'
	`, at, userID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return domain.ErrNotFound
	}
	if err := insertSession(ctx, tx, session); err != nil {
		return err
	}
	if err := appendAudit(ctx, tx, audit); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func insertSession(ctx context.Context, tx pgx.Tx, session domain.Session) error {
	_, err := tx.Exec(ctx, `
		insert into sessions (
			id, user_id, token_hash, csrf_secret_hash, created_at, last_seen_at,
			idle_expires_at, absolute_expires_at, ip_hash, user_agent_summary
		) values ($1, $2, $3, $4, $5, $5, $6, $7, $8, $9)
	`,
		session.ID,
		session.UserID,
		session.TokenHash,
		session.CSRFSecretHash,
		session.CreatedAt,
		session.IdleExpiresAt,
		session.AbsoluteExpiresAt,
		session.IPHash,
		session.UserAgentSummary,
	)
	return err
}

func (s *Store) Authenticate(
	ctx context.Context,
	tokenHash []byte,
	now time.Time,
	idleTTL time.Duration,
	expiredAudit domain.AuditEvent,
) (domain.AuthenticatedSession, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return domain.AuthenticatedSession{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var authenticated domain.AuthenticatedSession
	err = tx.QueryRow(ctx, `
		select
			u.id, u.username, u.display_name, u.password_hash, u.role, u.status,
			u.last_login_at, u.created_at, u.updated_at,
			s.id, s.user_id, s.token_hash, s.csrf_secret_hash, s.created_at,
			s.last_seen_at, s.idle_expires_at, s.absolute_expires_at, s.revoked_at,
			s.ip_hash, s.user_agent_summary
		from sessions s
		join users u on u.id = s.user_id
		where s.token_hash = $1 and s.revoked_at is null and u.status = 'ACTIVE'
		for update of s
	`, tokenHash).Scan(
		&authenticated.User.ID,
		&authenticated.User.Username,
		&authenticated.User.DisplayName,
		&authenticated.User.PasswordHash,
		&authenticated.User.Role,
		&authenticated.User.Status,
		&authenticated.User.LastLoginAt,
		&authenticated.User.CreatedAt,
		&authenticated.User.UpdatedAt,
		&authenticated.Session.ID,
		&authenticated.Session.UserID,
		&authenticated.Session.TokenHash,
		&authenticated.Session.CSRFSecretHash,
		&authenticated.Session.CreatedAt,
		&authenticated.Session.LastSeenAt,
		&authenticated.Session.IdleExpiresAt,
		&authenticated.Session.AbsoluteExpiresAt,
		&authenticated.Session.RevokedAt,
		&authenticated.Session.IPHash,
		&authenticated.Session.UserAgentSummary,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.AuthenticatedSession{}, domain.ErrNotFound
	}
	if err != nil {
		return domain.AuthenticatedSession{}, err
	}

	if !now.Before(authenticated.Session.IdleExpiresAt) || !now.Before(authenticated.Session.AbsoluteExpiresAt) {
		_, err = tx.Exec(ctx, "update sessions set revoked_at = $1 where id = $2", now, authenticated.Session.ID)
		if err != nil {
			return domain.AuthenticatedSession{}, err
		}
		expiredAudit.ActorType = domain.ActorUser
		expiredAudit.ActorID = authenticated.User.ID
		expiredAudit.Action = "AUTH_SESSION_EXPIRED"
		expiredAudit.ResourceType = "SESSION"
		expiredAudit.ResourceID = authenticated.Session.ID
		expiredAudit.Result = domain.AuditDenied
		expiredAudit.ReasonCode = "SESSION_EXPIRED"
		if err := appendAudit(ctx, tx, expiredAudit); err != nil {
			return domain.AuthenticatedSession{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return domain.AuthenticatedSession{}, err
		}
		return domain.AuthenticatedSession{}, domain.ErrSessionExpired
	}

	newIdleExpiry := now.Add(idleTTL)
	if newIdleExpiry.After(authenticated.Session.AbsoluteExpiresAt) {
		newIdleExpiry = authenticated.Session.AbsoluteExpiresAt
	}
	_, err = tx.Exec(ctx, `
		update sessions
		set last_seen_at = $1, idle_expires_at = $2
		where id = $3
	`, now, newIdleExpiry, authenticated.Session.ID)
	if err != nil {
		return domain.AuthenticatedSession{}, err
	}
	authenticated.Session.LastSeenAt = now
	authenticated.Session.IdleExpiresAt = newIdleExpiry
	if err := tx.Commit(ctx); err != nil {
		return domain.AuthenticatedSession{}, err
	}
	return authenticated, nil
}

func (s *Store) Logout(ctx context.Context, sessionID uuid.UUID, at time.Time, audit domain.AuditEvent) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx, `
		update sessions
		set revoked_at = $1
		where id = $2 and revoked_at is null
	`, at, sessionID)
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

func (s *Store) RecordAudit(ctx context.Context, event domain.AuditEvent) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := appendAudit(ctx, tx, event); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) AuditEvents(ctx context.Context) ([]domain.AuditEvent, error) {
	rows, err := s.pool.Query(ctx, `
		select sequence, id, occurred_at, actor_type, coalesce(actor_id::text, ''),
		       action, resource_type, coalesce(resource_id::text, ''), request_id,
		       source_ip_hash, result, reason_code, changes, previous_hash, event_hash
		from audit_events
		order by sequence
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var events []domain.AuditEvent
	for rows.Next() {
		var event domain.AuditEvent
		var actorID, resourceID string
		if err := rows.Scan(
			&event.Sequence,
			&event.ID,
			&event.OccurredAt,
			&event.ActorType,
			&actorID,
			&event.Action,
			&event.ResourceType,
			&resourceID,
			&event.RequestID,
			&event.SourceIPHash,
			&event.Result,
			&event.ReasonCode,
			&event.Changes,
			&event.PreviousHash,
			&event.EventHash,
		); err != nil {
			return nil, err
		}
		if actorID != "" {
			event.ActorID, err = uuid.Parse(actorID)
			if err != nil {
				return nil, err
			}
		}
		if resourceID != "" {
			event.ResourceID, err = uuid.Parse(resourceID)
			if err != nil {
				return nil, err
			}
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

func (s *Store) VerifyAuditChain(ctx context.Context) error {
	events, err := s.AuditEvents(ctx)
	if err != nil {
		return err
	}
	return domain.VerifyAuditChain(events)
}

func appendAudit(ctx context.Context, tx pgx.Tx, event domain.AuditEvent) error {
	event.OccurredAt = event.OccurredAt.UTC().Truncate(time.Microsecond)
	if event.ID == uuid.Nil {
		id, err := uuid.NewV7()
		if err != nil {
			return err
		}
		event.ID = id
	}
	if len(event.Changes) == 0 {
		event.Changes = json.RawMessage("{}")
	}
	if _, err := tx.Exec(ctx, "select pg_advisory_xact_lock($1)", int64(6740481908301)); err != nil {
		return err
	}

	var previous []byte
	err := tx.QueryRow(ctx, "select event_hash from audit_events order by sequence desc limit 1").Scan(&previous)
	if errors.Is(err, pgx.ErrNoRows) {
		previous = []byte{}
	} else if err != nil {
		return err
	}
	hash, err := domain.ComputeAuditHash(event, previous)
	if err != nil {
		return err
	}

	_, err = tx.Exec(ctx, `
		insert into audit_events (
			id, occurred_at, actor_type, actor_id, action, resource_type, resource_id,
			request_id, source_ip_hash, result, reason_code, changes, previous_hash, event_hash
		) values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
	`,
		event.ID,
		event.OccurredAt,
		event.ActorType,
		nullableUUID(event.ActorID),
		event.Action,
		event.ResourceType,
		nullableUUID(event.ResourceID),
		event.RequestID,
		nullableBytes(event.SourceIPHash),
		event.Result,
		event.ReasonCode,
		event.Changes,
		previous,
		hash,
	)
	return err
}

func nullableUUID(value uuid.UUID) any {
	if value == uuid.Nil {
		return nil
	}
	return value
}

func nullableBytes(value []byte) any {
	if len(value) == 0 {
		return nil
	}
	return value
}
