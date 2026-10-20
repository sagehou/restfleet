package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/sagehou/restfleet/internal/domain"
)

const planColumns = "id,name,host_id,agent_id,repository_id,template_id,template_revision,overrides,effective_config,config_hash,status,desired_revision,accepted_revision,backup_health,archived_at,created_at,updated_at"

func scanPlan(row rowScanner) (domain.Plan, error) {
	var p domain.Plan
	var overrides, effective []byte
	err := row.Scan(&p.ID, &p.Name, &p.HostID, &p.AgentID, &p.RepositoryID, &p.TemplateID,
		&p.TemplateRevision, &overrides, &effective, &p.ConfigHash, &p.Status,
		&p.DesiredRevision, &p.AcceptedRevision, &p.BackupHealth, &p.ArchivedAt, &p.CreatedAt, &p.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return p, domain.ErrNotFound
	}
	if err != nil {
		return p, err
	}
	p.Overrides = overrides
	if err := json.Unmarshal(effective, &p.EffectiveConfig); err != nil {
		return domain.Plan{}, err
	}
	return p, nil
}

func (s *Store) Plan(ctx context.Context, id uuid.UUID) (domain.Plan, error) {
	return scanPlan(s.pool.QueryRow(ctx, "select "+planColumns+" from plans where id=$1", id))
}

func (s *Store) Plans(ctx context.Context, after uuid.UUID, limit int) ([]domain.Plan, error) {
	rows, err := s.pool.Query(ctx, "select "+planColumns+" from plans where archived_at is null and id>$1 order by id limit $2", after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]domain.Plan, 0)
	for rows.Next() {
		p, err := scanPlan(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, p)
	}
	return items, rows.Err()
}

// CreatePlan verifies Host/Agent/Repository binding in the same transaction
// that writes the plan and its first revision snapshot.
func (s *Store) CreatePlan(ctx context.Context, p domain.Plan, audit domain.AuditEvent) (domain.Plan, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return domain.Plan{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var hostID uuid.UUID
	var agentStatus string
	err = tx.QueryRow(ctx, "select host_id,status from agents where id=$1 for update", p.AgentID).Scan(&hostID, &agentStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Plan{}, domain.ErrNotFound
	}
	if err != nil {
		return domain.Plan{}, err
	}
	if hostID != p.HostID || agentStatus != "ACTIVE" {
		return domain.Plan{}, domain.ErrPlanBindingInvalid
	}
	if err = tx.QueryRow(ctx, "select host_id from repositories where id=$1 and archived_at is null for update", p.RepositoryID).Scan(&hostID); errors.Is(err, pgx.ErrNoRows) {
		return domain.Plan{}, domain.ErrNotFound
	} else if err != nil {
		return domain.Plan{}, err
	}
	if hostID != p.HostID {
		return domain.Plan{}, domain.ErrPlanBindingInvalid
	}
	var exists bool
	if err = tx.QueryRow(ctx, "select exists(select 1 from template_revisions where template_id=$1 and revision=$2)", p.TemplateID, p.TemplateRevision).Scan(&exists); err != nil {
		return domain.Plan{}, err
	}
	if !exists {
		return domain.Plan{}, domain.ErrPlanTemplateStale
	}
	if err = tx.QueryRow(ctx, "select exists(select 1 from plans where host_id=$1 and lower(name)=lower($2) and archived_at is null)", p.HostID, p.Name).Scan(&exists); err != nil {
		return domain.Plan{}, err
	}
	if exists {
		return domain.Plan{}, domain.ErrPlanNameTaken
	}
	effective, err := json.Marshal(p.EffectiveConfig)
	if err != nil {
		return domain.Plan{}, err
	}
	if _, err = tx.Exec(ctx, `insert into plans(id,name,host_id,agent_id,repository_id,template_id,template_revision,
		overrides,effective_config,config_hash,status,desired_revision,accepted_revision,backup_health,created_at,updated_at)
		values($1,$2,$3,$4,$5,$6,$7,$8::jsonb,$9::jsonb,$10,$11,1,0,'UNKNOWN',$12,$12)`,
		p.ID, p.Name, p.HostID, p.AgentID, p.RepositoryID, p.TemplateID, p.TemplateRevision,
		string(p.Overrides), string(effective), p.ConfigHash, p.Status, p.CreatedAt); err != nil {
		return domain.Plan{}, persistenceError(err)
	}
	if _, err = tx.Exec(ctx, "insert into plan_revisions(plan_id,revision,effective_config_json,config_hash,created_by,created_at) values($1,1,$2::jsonb,$3,$4,$5)",
		p.ID, string(effective), p.ConfigHash, audit.ActorID, p.CreatedAt); err != nil {
		return domain.Plan{}, persistenceError(err)
	}
	if err = appendAudit(ctx, tx, audit); err != nil {
		return domain.Plan{}, err
	}
	p.DesiredRevision = 1
	if err = tx.Commit(ctx); err != nil {
		return domain.Plan{}, err
	}
	return p, nil
}

// SavePlanRevision appends the next desired revision. Applied revisions stay
// untouched until the Agent acknowledges the new one.
func (s *Store) SavePlanRevision(ctx context.Context, p domain.Plan, expectedRevision int64, audit domain.AuditEvent) (domain.Plan, error) {
	effective, err := json.Marshal(p.EffectiveConfig)
	if err != nil {
		return domain.Plan{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return domain.Plan{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var current int64
	var archivedAt *time.Time
	err = tx.QueryRow(ctx, "select desired_revision,archived_at from plans where id=$1 for update", p.ID).Scan(&current, &archivedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Plan{}, domain.ErrNotFound
	}
	if err != nil {
		return domain.Plan{}, err
	}
	if archivedAt != nil {
		return domain.Plan{}, domain.ErrNotFound
	}
	if current != expectedRevision {
		return domain.Plan{}, domain.ErrRevisionConflict
	}
	var exists bool
	if err = tx.QueryRow(ctx, "select exists(select 1 from template_revisions where template_id=$1 and revision=$2)", p.TemplateID, p.TemplateRevision).Scan(&exists); err != nil {
		return domain.Plan{}, err
	}
	if !exists {
		return domain.Plan{}, domain.ErrPlanTemplateStale
	}
	next := current + 1
	if _, err = tx.Exec(ctx, `insert into plan_revisions(plan_id,revision,effective_config_json,config_hash,created_by,created_at)
		values($1,$2,$3::jsonb,$4,$5,$6)`, p.ID, next, string(effective), p.ConfigHash, audit.ActorID, p.UpdatedAt); err != nil {
		return domain.Plan{}, persistenceError(err)
	}
	if _, err = tx.Exec(ctx, `update plans set name=$2,template_id=$3,template_revision=$4,overrides=$5::jsonb,
		effective_config=$6::jsonb,config_hash=$7,status=$8,desired_revision=$9,updated_at=$10 where id=$1`,
		p.ID, p.Name, p.TemplateID, p.TemplateRevision, string(p.Overrides), string(effective), p.ConfigHash, p.Status, next, p.UpdatedAt); err != nil {
		return domain.Plan{}, persistenceError(err)
	}
	if err = appendAudit(ctx, tx, audit); err != nil {
		return domain.Plan{}, err
	}
	p.DesiredRevision = next
	if err = tx.Commit(ctx); err != nil {
		return domain.Plan{}, err
	}
	return p, nil
}
