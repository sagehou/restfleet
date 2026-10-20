package server

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/sagehou/restfleet/internal/domain"
)

func (c *ControlPlane) Templates(ctx context.Context, after uuid.UUID, limit int) ([]domain.Template, error) {
	return c.store.Templates(ctx, after, limit)
}

func (c *ControlPlane) Template(ctx context.Context, id uuid.UUID) (domain.Template, error) {
	return c.store.Template(ctx, id)
}

// CreateTemplate validates the first revision before any row is written. The
// template pointer and its body are committed together by the store.
func (c *ControlPlane) CreateTemplate(ctx context.Context, name, description string, config domain.TemplateConfig, actor domain.User, meta RequestMeta) (domain.Template, error) {
	if actor.Role != domain.RoleAdmin {
		if err := c.RecordDenied(ctx, "TEMPLATE_CREATE", "TEMPLATE", "ROLE_DENIED", meta); err != nil {
			return domain.Template{}, err
		}
		return domain.Template{}, ErrForbidden
	}
	name = strings.TrimSpace(name)
	if !validTemplateText(name, 1, 128) {
		return domain.Template{}, &ValidationError{Field: "name", Code: "INVALID_NAME"}
	}
	description = strings.TrimSpace(description)
	if !validTemplateText(description, 0, 1024) {
		return domain.Template{}, &ValidationError{Field: "description", Code: "INVALID_DESCRIPTION"}
	}
	if err := config.Validate(); err != nil {
		return domain.Template{}, templateValidation(err)
	}
	id, err := uuid.NewV7()
	if err != nil {
		return domain.Template{}, err
	}
	now := c.clock().UTC()
	template := domain.Template{ID: id, Name: name, Description: description, CurrentRevision: 1,
		Config: config, CreatedAt: now, UpdatedAt: now}
	audit, err := c.userAudit("TEMPLATE_CREATE", "TEMPLATE", id, actor.ID, meta, "ACTIVE")
	if err != nil {
		return domain.Template{}, err
	}
	audit.Changes = json.RawMessage(`{"revision":1}`)
	return c.store.CreateTemplate(ctx, template, audit)
}

// UpdateTemplate appends a revision. Nothing is applied to existing Plans; that
// stays an explicit operator action.
func (c *ControlPlane) UpdateTemplate(ctx context.Context, id uuid.UUID, expectedRevision int64, description *string, config *domain.TemplateConfig, actor domain.User, meta RequestMeta) (domain.Template, error) {
	if actor.Role != domain.RoleAdmin {
		if err := c.RecordDenied(ctx, "TEMPLATE_UPDATE", "TEMPLATE", "ROLE_DENIED", meta); err != nil {
			return domain.Template{}, err
		}
		return domain.Template{}, ErrForbidden
	}
	if id.Version() != 7 || id.Variant() != uuid.RFC4122 {
		return domain.Template{}, &ValidationError{Field: "id", Code: "INVALID_ID"}
	}
	if expectedRevision < 1 {
		return domain.Template{}, &ValidationError{Field: "revision", Code: "INVALID_REVISION"}
	}
	current, err := c.store.Template(ctx, id)
	if err != nil {
		return domain.Template{}, err
	}
	if description != nil {
		trimmed := strings.TrimSpace(*description)
		if !validTemplateText(trimmed, 0, 1024) {
			return domain.Template{}, &ValidationError{Field: "description", Code: "INVALID_DESCRIPTION"}
		}
		current.Description = trimmed
	}
	if config != nil {
		if err := config.Validate(); err != nil {
			return domain.Template{}, templateValidation(err)
		}
		current.Config = *config
	}
	current.UpdatedAt = c.clock().UTC()
	audit, err := c.userAudit("TEMPLATE_UPDATE", "TEMPLATE", id, actor.ID, meta, "ACTIVE")
	if err != nil {
		return domain.Template{}, err
	}
	changes, err := json.Marshal(map[string]int64{"revision": expectedRevision + 1})
	if err != nil {
		return domain.Template{}, err
	}
	audit.Changes = changes
	return c.store.SaveTemplateRevision(ctx, current, expectedRevision, audit)
}

func templateValidation(err error) error {
	var invalid *domain.TemplateError
	if errors.As(err, &invalid) {
		return &ValidationError{Field: invalid.Field, Code: invalid.Code}
	}
	return err
}

func validTemplateText(value string, min, max int) bool {
	if !utf8.ValidString(value) || strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return false
	}
	length := utf8.RuneCountInString(value)
	return length >= min && length <= max
}

// ArchiveTemplate is only allowed when no active plan still references it.
func (c *ControlPlane) ArchiveTemplate(ctx context.Context, id uuid.UUID, expectedRevision int64, actor domain.User, meta RequestMeta) (domain.Template, error) {
	if actor.Role != domain.RoleAdmin {
		if err := c.RecordDenied(ctx, "TEMPLATE_ARCHIVE", "TEMPLATE", "ROLE_DENIED", meta); err != nil {
			return domain.Template{}, err
		}
		return domain.Template{}, ErrForbidden
	}
	if id.Version() != 7 || id.Variant() != uuid.RFC4122 {
		return domain.Template{}, &ValidationError{Field: "id", Code: "INVALID_ID"}
	}
	if expectedRevision < 1 {
		return domain.Template{}, &ValidationError{Field: "revision", Code: "INVALID_REVISION"}
	}
	audit, err := c.userAudit("TEMPLATE_ARCHIVE", "TEMPLATE", id, actor.ID, meta, "ARCHIVED")
	if err != nil {
		return domain.Template{}, err
	}
	audit.Changes = json.RawMessage(`{"archived":true}`)
	return c.store.ArchiveTemplate(ctx, id, expectedRevision, c.clock().UTC(), audit)
}

func (c *ControlPlane) DependentPlans(ctx context.Context, id uuid.UUID) ([]domain.Plan, error) {
	return c.store.DependentPlans(ctx, id)
}
