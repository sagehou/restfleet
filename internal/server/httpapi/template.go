package httpapi

import (
	"encoding/base64"
	"net/http"

	"github.com/google/uuid"
	openapi_types "github.com/oapi-codegen/runtime/types"
	"github.com/sagehou/restfleet/internal/domain"
)

func templateConfigResponse(c domain.TemplateConfig) TemplateConfig {
	exclude := append([]string{}, c.ExcludePatterns...)
	preHooks := append([]openapi_types.UUID{}, c.PreHookIDs...)
	postHooks := append([]openapi_types.UUID{}, c.PostHookIDs...)
	enabled := c.EnabledByDefault
	excludeCaches := c.ExcludeCaches
	oneFileSystem := c.OneFileSystem
	jitter := c.JitterSeconds
	misfire := c.MisfireGraceSeconds
	config := TemplateConfig{
		Paths:               append([]string{}, c.Paths...),
		ExcludePatterns:     &exclude,
		PreHookIds:          &preHooks,
		PostHookIds:         &postHooks,
		EnabledByDefault:    &enabled,
		ExcludeCaches:       &excludeCaches,
		OneFileSystem:       &oneFileSystem,
		JitterSeconds:       &jitter,
		MisfireGraceSeconds: &misfire,
		ScheduleCron:        c.ScheduleCron,
		ScheduleTimezone:    c.ScheduleTimezone,
	}
	if c.RetryPolicy != nil {
		retry := c.RetryPolicy
		config.RetryPolicy = &retry
	}
	if c.ResourceLimits != nil {
		limits := c.ResourceLimits
		config.ResourceLimits = &limits
	}
	if c.RetentionPolicyID != nil {
		retention := *c.RetentionPolicyID
		config.RetentionPolicyId = &retention
	}
	return config
}

func templateConfigRequest(request TemplateConfig) domain.TemplateConfig {
	config := domain.TemplateConfig{
		Paths:            append([]string{}, request.Paths...),
		ScheduleCron:     request.ScheduleCron,
		ScheduleTimezone: request.ScheduleTimezone,
	}
	if request.ExcludePatterns != nil {
		config.ExcludePatterns = append([]string{}, (*request.ExcludePatterns)...)
	}
	if request.PreHookIds != nil {
		config.PreHookIDs = append([]uuid.UUID{}, (*request.PreHookIds)...)
	}
	if request.PostHookIds != nil {
		config.PostHookIDs = append([]uuid.UUID{}, (*request.PostHookIds)...)
	}
	if request.ExcludeCaches != nil {
		config.ExcludeCaches = *request.ExcludeCaches
	}
	if request.OneFileSystem != nil {
		config.OneFileSystem = *request.OneFileSystem
	}
	if request.JitterSeconds != nil {
		config.JitterSeconds = *request.JitterSeconds
	}
	config.MisfireGraceSeconds = 900
	if request.MisfireGraceSeconds != nil {
		config.MisfireGraceSeconds = *request.MisfireGraceSeconds
	}
	config.EnabledByDefault = true
	if request.EnabledByDefault != nil {
		config.EnabledByDefault = *request.EnabledByDefault
	}
	if request.RetryPolicy != nil {
		config.RetryPolicy = *request.RetryPolicy
	}
	if request.ResourceLimits != nil {
		config.ResourceLimits = *request.ResourceLimits
	}
	if request.RetentionPolicyId != nil {
		retention := *request.RetentionPolicyId
		config.RetentionPolicyID = &retention
	}
	return config
}

func templateResponse(t domain.Template) Template {
	return Template{Id: t.ID, Name: t.Name, Description: t.Description, Revision: t.CurrentRevision,
		Config: templateConfigResponse(t.Config), CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt}
}

func (a *API) ListTemplates(w http.ResponseWriter, r *http.Request, params ListTemplatesParams) {
	if _, ok := a.authorizeRead(w, r); !ok {
		return
	}
	after, limit, ok := a.listPage(w, r, params.Limit, params.Cursor)
	if !ok {
		return
	}
	items, err := a.control.Templates(r.Context(), after, limit+1)
	if err != nil {
		a.fleetProblem(w, r, err)
		return
	}
	response := TemplateList{Items: make([]Template, 0)}
	if len(items) > limit {
		last := items[limit-1].ID
		cursor := base64.RawURLEncoding.EncodeToString(last[:])
		response.NextCursor = &cursor
		items = items[:limit]
	}
	for _, template := range items {
		response.Items = append(response.Items, templateResponse(template))
	}
	a.json(w, http.StatusOK, response)
}

func (a *API) GetTemplate(w http.ResponseWriter, r *http.Request, templateId openapi_types.UUID) {
	if _, ok := a.authorizeRead(w, r); !ok {
		return
	}
	if r.URL.RawQuery != "" {
		a.invalidTemplateRequest(w, r)
		return
	}
	template, err := a.control.Template(r.Context(), templateId)
	if err != nil {
		a.fleetProblem(w, r, err)
		return
	}
	setETag(w, template.CurrentRevision)
	a.json(w, http.StatusOK, templateResponse(template))
}

func (a *API) CreateTemplate(w http.ResponseWriter, r *http.Request, params CreateTemplateParams) {
	actor, ok := a.authorizeMutation(w, r, params.XCSRFToken, "TEMPLATE_CREATE", "TEMPLATE")
	if !ok {
		return
	}
	if r.URL.RawQuery != "" {
		a.invalidTemplateRequest(w, r)
		return
	}
	var request TemplateCreate
	if err := decodeJSON(w, r, &request); err != nil {
		a.invalidTemplateRequest(w, r)
		return
	}
	description := ""
	if request.Description != nil {
		description = *request.Description
	}
	template, err := a.control.CreateTemplate(r.Context(), request.Name, description,
		templateConfigRequest(request.Config), actor.User, requestMeta(r))
	if err != nil {
		a.fleetProblem(w, r, err)
		return
	}
	setETag(w, template.CurrentRevision)
	w.Header().Set("Location", "/api/v1/templates/"+template.ID.String())
	a.json(w, http.StatusCreated, templateResponse(template))
}

func (a *API) UpdateTemplate(w http.ResponseWriter, r *http.Request, templateId openapi_types.UUID, params UpdateTemplateParams) {
	actor, ok := a.authorizeMutation(w, r, params.XCSRFToken, "TEMPLATE_UPDATE", "TEMPLATE")
	if !ok {
		return
	}
	if r.URL.RawQuery != "" {
		a.invalidTemplateRequest(w, r)
		return
	}
	revision, err := parseETag(params.IfMatch)
	if err != nil {
		a.problem(w, r, http.StatusBadRequest, "INVALID_REVISION", "Invalid request", "If-Match must contain a positive revision.", nil)
		return
	}
	var request TemplateUpdate
	if err := decodeJSON(w, r, &request); err != nil {
		a.invalidTemplateRequest(w, r)
		return
	}
	var config *domain.TemplateConfig
	if request.Config != nil {
		converted := templateConfigRequest(*request.Config)
		config = &converted
	}
	template, err := a.control.UpdateTemplate(r.Context(), templateId, revision, request.Description, config, actor.User, requestMeta(r))
	if err != nil {
		a.fleetProblem(w, r, err)
		return
	}
	setETag(w, template.CurrentRevision)
	a.json(w, http.StatusOK, templateResponse(template))
}

func (a *API) invalidTemplateRequest(w http.ResponseWriter, r *http.Request) {
	if err := a.control.RecordDenied(r.Context(), "TEMPLATE_REQUEST", "TEMPLATE", "INVALID_REQUEST", requestMeta(r)); err != nil {
		a.internalProblem(w, r)
		return
	}
	a.problem(w, r, http.StatusBadRequest, "INVALID_REQUEST", "Invalid request", "The template request is invalid.", nil)
}
