package server

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/google/uuid"
	"github.com/sagehou/restfleet/internal/domain"
)

func (c *ControlPlane) Plans(ctx context.Context, after uuid.UUID, limit int) ([]domain.Plan, error) {
	return c.store.Plans(ctx, after, limit)
}

func (c *ControlPlane) Plan(ctx context.Context, id uuid.UUID) (domain.Plan, error) {
	return c.store.Plan(ctx, id)
}

// CreatePlan snapshots the merged effective config. It never activates the plan
// and never mutates the template.
func (c *ControlPlane) CreatePlan(ctx context.Context, name string, hostID, agentID, repositoryID, templateID uuid.UUID,
	templateRevision int64, overrides json.RawMessage, actor domain.User, meta RequestMeta) (domain.Plan, error) {
	if actor.Role != domain.RoleAdmin {
		if err := c.RecordDenied(ctx, "PLAN_CREATE", "PLAN", "ROLE_DENIED", meta); err != nil {
			return domain.Plan{}, err
		}
		return domain.Plan{}, ErrForbidden
	}
	name = strings.TrimSpace(name)
	if !validTemplateText(name, 1, 128) {
		return domain.Plan{}, &ValidationError{Field: "name", Code: "INVALID_NAME"}
	}
	for field, id := range map[string]uuid.UUID{"host_id": hostID, "agent_id": agentID, "repository_id": repositoryID, "template_id": templateID} {
		if id.Version() != 7 || id.Variant() != uuid.RFC4122 {
			return domain.Plan{}, &ValidationError{Field: field, Code: "INVALID_ID"}
		}
	}
	template, err := c.store.Template(ctx, templateID)
	if err != nil {
		return domain.Plan{}, err
	}
	if template.Archived() {
		return domain.Plan{}, &ValidationError{Field: "template_id", Code: "TEMPLATE_ARCHIVED"}
	}
	if templateRevision < 1 || templateRevision > template.CurrentRevision {
		return domain.Plan{}, &ValidationError{Field: "template_revision", Code: "INVALID_REVISION"}
	}
	base, err := c.store.TemplateRevisionConfig(ctx, templateID, templateRevision)
	if err != nil {
		return domain.Plan{}, err
	}
	config, err := domain.MergePlanConfig(base, overrides)
	if err != nil {
		return domain.Plan{}, templateValidation(err)
	}
	normalized, err := domain.NormalizeOverrides(overrides)
	if err != nil {
		return domain.Plan{}, templateValidation(err)
	}
	hash, err := config.ConfigHash()
	if err != nil {
		return domain.Plan{}, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return domain.Plan{}, err
	}
	now := c.clock().UTC()
	plan := domain.Plan{ID: id, Name: name, HostID: hostID, AgentID: agentID, RepositoryID: repositoryID,
		TemplateID: templateID, TemplateRevision: templateRevision, Overrides: normalized,
		EffectiveConfig: config, ConfigHash: hash, Status: "DRAFT", DesiredRevision: 1,
		BackupHealth: "UNKNOWN", CreatedAt: now, UpdatedAt: now}
	audit, err := c.userAudit("PLAN_CREATE", "PLAN", id, actor.ID, meta, "DRAFT")
	if err != nil {
		return domain.Plan{}, err
	}
	audit.Changes = json.RawMessage(`{"revision":1}`)
	return c.store.CreatePlan(ctx, plan, audit)
}

// UpdatePlan appends a desired revision. A change to the config or template
// revision leaves an ACTIVE plan in PENDING_APPLY until an explicit apply.
func (c *ControlPlane) UpdatePlan(ctx context.Context, id uuid.UUID, expectedRevision int64, name *string,
	templateRevision *int64, overrides *json.RawMessage, actor domain.User, meta RequestMeta) (domain.Plan, error) {
	if actor.Role != domain.RoleAdmin {
		if err := c.RecordDenied(ctx, "PLAN_UPDATE", "PLAN", "ROLE_DENIED", meta); err != nil {
			return domain.Plan{}, err
		}
		return domain.Plan{}, ErrForbidden
	}
	if id.Version() != 7 || id.Variant() != uuid.RFC4122 {
		return domain.Plan{}, &ValidationError{Field: "id", Code: "INVALID_ID"}
	}
	if expectedRevision < 1 {
		return domain.Plan{}, &ValidationError{Field: "revision", Code: "INVALID_REVISION"}
	}
	current, err := c.store.Plan(ctx, id)
	if err != nil {
		return domain.Plan{}, err
	}
	if name != nil {
		trimmed := strings.TrimSpace(*name)
		if !validTemplateText(trimmed, 1, 128) {
			return domain.Plan{}, &ValidationError{Field: "name", Code: "INVALID_NAME"}
		}
		current.Name = trimmed
	}
	configChanged := overrides != nil
	if overrides != nil {
		normalized, err := domain.NormalizeOverrides(*overrides)
		if err != nil {
			return domain.Plan{}, templateValidation(err)
		}
		current.Overrides = normalized
	}
	templateChanged := false
	if templateRevision != nil {
		if *templateRevision < 1 {
			return domain.Plan{}, &ValidationError{Field: "template_revision", Code: "INVALID_REVISION"}
		}
		template, err := c.store.Template(ctx, current.TemplateID)
		if err != nil {
			return domain.Plan{}, err
		}
		if *templateRevision > template.CurrentRevision {
			return domain.Plan{}, &ValidationError{Field: "template_revision", Code: "INVALID_REVISION"}
		}
		templateChanged = *templateRevision != current.TemplateRevision
		current.TemplateRevision = *templateRevision
	}
	if configChanged || templateChanged {
		base, err := c.store.TemplateRevisionConfig(ctx, current.TemplateID, current.TemplateRevision)
		if err != nil {
			return domain.Plan{}, err
		}
		config, err := domain.MergePlanConfig(base, current.Overrides)
		if err != nil {
			return domain.Plan{}, templateValidation(err)
		}
		hash, err := config.ConfigHash()
		if err != nil {
			return domain.Plan{}, err
		}
		current.EffectiveConfig, current.ConfigHash = config, hash
		if current.Status == "ACTIVE" || current.Status == "PAUSED" {
			current.Status = "PENDING_APPLY"
		}
	}
	current.UpdatedAt = c.clock().UTC()
	audit, err := c.userAudit("PLAN_UPDATE", "PLAN", id, actor.ID, meta, current.Status)
	if err != nil {
		return domain.Plan{}, err
	}
	changes, err := json.Marshal(map[string]int64{"revision": expectedRevision + 1})
	if err != nil {
		return domain.Plan{}, err
	}
	audit.Changes = changes
	return c.store.SavePlanRevision(ctx, current, expectedRevision, audit)
}
