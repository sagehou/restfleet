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

const templateColumns = "t.id,t.name,t.description,t.current_revision,t.archived_at,t.created_at,t.updated_at,r.config_json"

func scanTemplate(row rowScanner) (domain.Template, error) {
	var t domain.Template
	var config []byte
	err := row.Scan(&t.ID, &t.Name, &t.Description, &t.CurrentRevision, &t.ArchivedAt, &t.CreatedAt, &t.UpdatedAt, &config)
	if errors.Is(err, pgx.ErrNoRows) {
		return t, domain.ErrNotFound
	}
	if err != nil {
		return t, err
	}
	if err := json.Unmarshal(config, &t.Config); err != nil {
		return domain.Template{}, err
	}
	return t, nil
}

func (s *Store) Template(ctx context.Context, id uuid.UUID) (domain.Template, error) {
	return scanTemplate(s.pool.QueryRow(ctx, "select "+templateColumns+
		" from templates t join template_revisions r on r.template_id=t.id and r.revision=t.current_revision where t.id=$1", id))
}

func (s *Store) Templates(ctx context.Context, after uuid.UUID, limit int) ([]domain.Template, error) {
	rows, err := s.pool.Query(ctx, "select "+templateColumns+
		" from templates t join template_revisions r on r.template_id=t.id and r.revision=t.current_revision"+
		" where t.archived_at is null and t.id>$1 order by t.id limit $2", after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]domain.Template, 0)
	for rows.Next() {
		t, err := scanTemplate(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, t)
	}
	return items, rows.Err()
}

// CreateTemplate writes the pointer and its first immutable revision in one
// transaction so a template never exists without a body.
func (s *Store) CreateTemplate(ctx context.Context, t domain.Template, audit domain.AuditEvent) (domain.Template, error) {
	canonical, err := t.Config.CanonicalJSON()
	if err != nil {
		return domain.Template{}, err
	}
	hash, err := t.Config.ConfigHash()
	if err != nil {
		return domain.Template{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return domain.Template{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var exists bool
	if err = tx.QueryRow(ctx, "select exists(select 1 from templates where lower(name)=lower($1) and archived_at is null)", t.Name).Scan(&exists); err != nil {
		return domain.Template{}, err
	}
	if exists {
		return domain.Template{}, domain.ErrTemplateNameTaken
	}
	if _, err = tx.Exec(ctx, "insert into templates(id,name,description,current_revision,created_at,updated_at) values($1,$2,$3,1,$4,$4)",
		t.ID, t.Name, t.Description, t.CreatedAt); err != nil {
		return domain.Template{}, persistenceError(err)
	}
	if _, err = tx.Exec(ctx, "insert into template_revisions(template_id,revision,config_json,config_hash,created_by,created_at) values($1,1,$2::jsonb,$3,$4,$5)",
		t.ID, string(canonical), hash, audit.ActorID, t.CreatedAt); err != nil {
		return domain.Template{}, persistenceError(err)
	}
	if err = appendAudit(ctx, tx, audit); err != nil {
		return domain.Template{}, err
	}
	t.CurrentRevision = 1
	if err = tx.Commit(ctx); err != nil {
		return domain.Template{}, err
	}
	return t, nil
}

// SaveTemplateRevision appends revision current+1. The row lock keeps
// concurrent writers from silently overwriting each other's revision.
func (s *Store) SaveTemplateRevision(ctx context.Context, t domain.Template, expectedRevision int64, audit domain.AuditEvent) (domain.Template, error) {
	canonical, err := t.Config.CanonicalJSON()
	if err != nil {
		return domain.Template{}, err
	}
	hash, err := t.Config.ConfigHash()
	if err != nil {
		return domain.Template{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return domain.Template{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var current int64
	var archivedAt *time.Time
	err = tx.QueryRow(ctx, "select current_revision,archived_at from templates where id=$1 for update", t.ID).Scan(&current, &archivedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Template{}, domain.ErrNotFound
	}
	if err != nil {
		return domain.Template{}, err
	}
	if archivedAt != nil {
		return domain.Template{}, domain.ErrTemplateArchived
	}
	if current != expectedRevision {
		return domain.Template{}, domain.ErrRevisionConflict
	}
	next := current + 1
	if _, err = tx.Exec(ctx, "insert into template_revisions(template_id,revision,config_json,config_hash,created_by,created_at) values($1,$2,$3::jsonb,$4,$5,$6)",
		t.ID, next, string(canonical), hash, audit.ActorID, t.UpdatedAt); err != nil {
		return domain.Template{}, persistenceError(err)
	}
	if _, err = tx.Exec(ctx, "update templates set description=$2,current_revision=$3,updated_at=$4 where id=$1",
		t.ID, t.Description, next, t.UpdatedAt); err != nil {
		return domain.Template{}, persistenceError(err)
	}
	if err = appendAudit(ctx, tx, audit); err != nil {
		return domain.Template{}, err
	}
	t.CurrentRevision = next
	if err = tx.Commit(ctx); err != nil {
		return domain.Template{}, err
	}
	return t, nil
}

// TemplateRevisionConfig returns one immutable revision body for plan merging.
func (s *Store) TemplateRevisionConfig(ctx context.Context, templateID uuid.UUID, revision int64) (domain.TemplateConfig, error) {
	var raw []byte
	err := s.pool.QueryRow(ctx, "select config_json from template_revisions where template_id=$1 and revision=$2", templateID, revision).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.TemplateConfig{}, domain.ErrNotFound
	}
	if err != nil {
		return domain.TemplateConfig{}, err
	}
	var config domain.TemplateConfig
	if err := json.Unmarshal(raw, &config); err != nil {
		return domain.TemplateConfig{}, err
	}
	return config, nil
}

// DependentPlans lists non-archived plans that still reference the template.
func (s *Store) DependentPlans(ctx context.Context, templateID uuid.UUID) ([]domain.Plan, error) {
	rows, err := s.pool.Query(ctx, "select "+planColumns+" from plans where template_id=$1 and archived_at is null order by id", templateID)
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

// ArchiveTemplate refuses to hide a template that active plans still reference.
func (s *Store) ArchiveTemplate(ctx context.Context, id uuid.UUID, expectedRevision int64, now time.Time, audit domain.AuditEvent) (domain.Template, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return domain.Template{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var current int64
	var archivedAt *time.Time
	err = tx.QueryRow(ctx, "select current_revision,archived_at from templates where id=$1 for update", id).Scan(&current, &archivedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Template{}, domain.ErrNotFound
	}
	if err != nil {
		return domain.Template{}, err
	}
	if archivedAt != nil {
		return domain.Template{}, domain.ErrTemplateArchived
	}
	if current != expectedRevision {
		return domain.Template{}, domain.ErrRevisionConflict
	}
	var dependents int
	if err = tx.QueryRow(ctx, "select count(*) from plans where template_id=$1 and archived_at is null", id).Scan(&dependents); err != nil {
		return domain.Template{}, err
	}
	if dependents > 0 {
		return domain.Template{}, domain.ErrTemplateInUse
	}
	if _, err = tx.Exec(ctx, "update templates set archived_at=$2,updated_at=$2 where id=$1", id, now); err != nil {
		return domain.Template{}, err
	}
	if err = appendAudit(ctx, tx, audit); err != nil {
		return domain.Template{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return domain.Template{}, err
	}
	return s.Template(ctx, id)
}
