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

// PausePlan stops future scheduling without cancelling anything already running.
func (c *ControlPlane) PausePlan(ctx context.Context, id uuid.UUID, expectedRevision int64, actor domain.User, meta RequestMeta) (domain.Plan, error) {
	return c.planStatus(ctx, id, expectedRevision, "PAUSED", "PLAN_PAUSE", []string{"DRAFT", "PENDING_APPLY", "ACTIVE"}, actor, meta)
}

// ResumePlan re-activates a paused plan at its current desired revision.
func (c *ControlPlane) ResumePlan(ctx context.Context, id uuid.UUID, expectedRevision int64, actor domain.User, meta RequestMeta) (domain.Plan, error) {
	return c.planStatus(ctx, id, expectedRevision, "ACTIVE", "PLAN_RESUME", []string{"PAUSED"}, actor, meta)
}

// ApplyPlan confirms the current desired revision and moves the plan to ACTIVE.
func (c *ControlPlane) ApplyPlan(ctx context.Context, id uuid.UUID, expectedRevision int64, actor domain.User, meta RequestMeta) (domain.Plan, error) {
	return c.planStatus(ctx, id, expectedRevision, "ACTIVE", "PLAN_APPLY", []string{"DRAFT", "PENDING_APPLY", "INVALID"}, actor, meta)
}

func (c *ControlPlane) planStatus(ctx context.Context, id uuid.UUID, expectedRevision int64, target, action string,
	allowed []string, actor domain.User, meta RequestMeta) (domain.Plan, error) {
	if actor.Role != domain.RoleAdmin {
		if err := c.RecordDenied(ctx, action, "PLAN", "ROLE_DENIED", meta); err != nil {
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
	plan, err := c.store.Plan(ctx, id)
	if err != nil {
		return domain.Plan{}, err
	}
	permitted := false
	for _, status := range allowed {
		if plan.Status == status {
			permitted = true
			break
		}
	}
	if !permitted {
		return domain.Plan{}, domain.ErrPlanStateInvalid
	}
	plan.Status = target
	plan.UpdatedAt = c.clock().UTC()
	audit, err := c.userAudit(action, "PLAN", id, actor.ID, meta, target)
	if err != nil {
		return domain.Plan{}, err
	}
	changes, err := json.Marshal(map[string]int64{"revision": expectedRevision + 1})
	if err != nil {
		return domain.Plan{}, err
	}
	audit.Changes = changes
	return c.store.SavePlanRevision(ctx, plan, expectedRevision, audit)
}

// ApplyTemplateToPlans moves the named plans to one template revision. Every
// plan is handled on its own so one revision conflict cannot block the batch.
func (c *ControlPlane) ApplyTemplateToPlans(ctx context.Context, templateID uuid.UUID, templateRevision int64,
	targets []domain.PlanApplyTarget, actor domain.User, meta RequestMeta) ([]domain.PlanApplyResult, error) {
	if actor.Role != domain.RoleAdmin {
		if err := c.RecordDenied(ctx, "TEMPLATE_APPLY_TO_PLANS", "TEMPLATE", "ROLE_DENIED", meta); err != nil {
			return nil, err
		}
		return nil, ErrForbidden
	}
	if templateRevision < 1 {
		return nil, &ValidationError{Field: "template_revision", Code: "INVALID_REVISION"}
	}
	template, err := c.store.Template(ctx, templateID)
	if err != nil {
		return nil, err
	}
	if template.Archived() {
		return nil, &ValidationError{Field: "template_id", Code: "TEMPLATE_ARCHIVED"}
	}
	if templateRevision > template.CurrentRevision {
		return nil, &ValidationError{Field: "template_revision", Code: "INVALID_REVISION"}
	}
	base, err := c.store.TemplateRevisionConfig(ctx, templateID, templateRevision)
	if err != nil {
		return nil, err
	}
	results := make([]domain.PlanApplyResult, 0, len(targets))
	for _, target := range targets {
		result := domain.PlanApplyResult{PlanID: target.PlanID}
		plan, err := c.store.Plan(ctx, target.PlanID)
		switch {
		case err != nil:
			result.Status, result.Code = "CONFLICT", "NOT_FOUND"
		case plan.ArchivedAt != nil:
			result.Status, result.Code = "CONFLICT", "PLAN_ARCHIVED"
		case plan.TemplateID != templateID:
			result.Status, result.Code = "CONFLICT", "NOT_USING_TEMPLATE"
		case plan.DesiredRevision != target.ExpectedRevision:
			result.Status, result.Code = "CONFLICT", "REVISION_CONFLICT"
		default:
			config, mergeErr := domain.MergePlanConfig(base, plan.Overrides)
			if mergeErr != nil {
				result.Status, result.Code = "CONFLICT", "INVALID_CONFIG"
				break
			}
			hash, hashErr := config.ConfigHash()
			if hashErr != nil {
				return results, hashErr
			}
			plan.TemplateRevision = templateRevision
			plan.EffectiveConfig, plan.ConfigHash = config, hash
			if plan.Status == "ACTIVE" || plan.Status == "PAUSED" {
				plan.Status = "PENDING_APPLY"
			}
			plan.UpdatedAt = c.clock().UTC()
			audit, auditErr := c.userAudit("PLAN_TEMPLATE_APPLY", "PLAN", plan.ID, actor.ID, meta, plan.Status)
			if auditErr != nil {
				return results, auditErr
			}
			changes, _ := json.Marshal(map[string]int64{"revision": target.ExpectedRevision + 1, "template_revision": templateRevision})
			audit.Changes = changes
			if _, saveErr := c.store.SavePlanRevision(ctx, plan, target.ExpectedRevision, audit); saveErr != nil {
				result.Status = "CONFLICT"
				result.Code = "SAVE_FAILED"
				break
			}
			result.Status, result.Code = "APPLIED", ""
		}
		results = append(results, result)
	}
	return results, nil
}
