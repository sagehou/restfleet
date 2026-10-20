package httpapi

import (
	"encoding/base64"
	"encoding/json"
	"net/http"

	openapi_types "github.com/oapi-codegen/runtime/types"
	"github.com/sagehou/restfleet/internal/domain"
)

func planOverridesResponse(raw json.RawMessage) *map[string]interface{} {
	if len(raw) == 0 {
		return nil
	}
	var values map[string]interface{}
	if err := json.Unmarshal(raw, &values); err != nil || len(values) == 0 {
		return nil
	}
	return &values
}

func planOverridesRequest(values *map[string]interface{}) (json.RawMessage, error) {
	if values == nil {
		return json.RawMessage(`{}`), nil
	}
	return json.Marshal(*values)
}

func planResponse(p domain.Plan) Plan {
	status := PlanStatus(p.Status)
	health := PlanBackupHealth(p.BackupHealth)
	return Plan{Id: p.ID, Name: p.Name, HostId: p.HostID, AgentId: p.AgentID, RepositoryId: p.RepositoryID,
		TemplateId: p.TemplateID, TemplateRevision: p.TemplateRevision, Status: status,
		DesiredRevision: p.DesiredRevision, AcceptedRevision: p.AcceptedRevision, BackupHealth: health,
		Overrides: planOverridesResponse(p.Overrides), EffectiveConfig: templateConfigResponse(p.EffectiveConfig),
		CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt}
}

func (a *API) ListPlans(w http.ResponseWriter, r *http.Request, params ListPlansParams) {
	if _, ok := a.authorizeRead(w, r); !ok {
		return
	}
	after, limit, ok := a.listPage(w, r, params.Limit, params.Cursor)
	if !ok {
		return
	}
	items, err := a.control.Plans(r.Context(), after, limit+1)
	if err != nil {
		a.fleetProblem(w, r, err)
		return
	}
	response := PlanList{Items: make([]Plan, 0)}
	if len(items) > limit {
		last := items[limit-1].ID
		cursor := base64.RawURLEncoding.EncodeToString(last[:])
		response.NextCursor = &cursor
		items = items[:limit]
	}
	for _, plan := range items {
		response.Items = append(response.Items, planResponse(plan))
	}
	a.json(w, http.StatusOK, response)
}

func (a *API) GetPlan(w http.ResponseWriter, r *http.Request, planID openapi_types.UUID) {
	if _, ok := a.authorizeRead(w, r); !ok {
		return
	}
	if r.URL.RawQuery != "" {
		a.invalidPlanRequest(w, r)
		return
	}
	plan, err := a.control.Plan(r.Context(), planID)
	if err != nil {
		a.fleetProblem(w, r, err)
		return
	}
	setETag(w, plan.DesiredRevision)
	a.json(w, http.StatusOK, planResponse(plan))
}

func (a *API) GetPlanEffectiveConfig(w http.ResponseWriter, r *http.Request, planID openapi_types.UUID) {
	if _, ok := a.authorizeRead(w, r); !ok {
		return
	}
	if r.URL.RawQuery != "" {
		a.invalidPlanRequest(w, r)
		return
	}
	plan, err := a.control.Plan(r.Context(), planID)
	if err != nil {
		a.fleetProblem(w, r, err)
		return
	}
	setETag(w, plan.DesiredRevision)
	a.json(w, http.StatusOK, planResponse(plan))
}

func (a *API) CreatePlan(w http.ResponseWriter, r *http.Request, params CreatePlanParams) {
	actor, ok := a.authorizeMutation(w, r, params.XCSRFToken, "PLAN_CREATE", "PLAN")
	if !ok {
		return
	}
	if r.URL.RawQuery != "" {
		a.invalidPlanRequest(w, r)
		return
	}
	var request PlanCreate
	if err := decodeJSON(w, r, &request); err != nil {
		a.invalidPlanRequest(w, r)
		return
	}
	overrides, err := planOverridesRequest(request.Overrides)
	if err != nil {
		a.invalidPlanRequest(w, r)
		return
	}
	plan, err := a.control.CreatePlan(r.Context(), request.Name, request.HostId, request.AgentId,
		request.RepositoryId, request.TemplateId, request.TemplateRevision, overrides, actor.User, requestMeta(r))
	if err != nil {
		a.fleetProblem(w, r, err)
		return
	}
	setETag(w, plan.DesiredRevision)
	w.Header().Set("Location", "/api/v1/plans/"+plan.ID.String())
	a.json(w, http.StatusCreated, planResponse(plan))
}

func (a *API) UpdatePlan(w http.ResponseWriter, r *http.Request, planID openapi_types.UUID, params UpdatePlanParams) {
	actor, ok := a.authorizeMutation(w, r, params.XCSRFToken, "PLAN_UPDATE", "PLAN")
	if !ok {
		return
	}
	if r.URL.RawQuery != "" {
		a.invalidPlanRequest(w, r)
		return
	}
	revision, err := parseETag(params.IfMatch)
	if err != nil {
		a.problem(w, r, http.StatusBadRequest, "INVALID_REVISION", "Invalid request", "If-Match must contain a positive revision.", nil)
		return
	}
	var request PlanUpdate
	if err := decodeJSON(w, r, &request); err != nil {
		a.invalidPlanRequest(w, r)
		return
	}
	var overrides *json.RawMessage
	if request.Overrides != nil {
		raw, err := planOverridesRequest(request.Overrides)
		if err != nil {
			a.invalidPlanRequest(w, r)
			return
		}
		overrides = &raw
	}
	plan, err := a.control.UpdatePlan(r.Context(), planID, revision, request.Name, request.TemplateRevision, overrides, actor.User, requestMeta(r))
	if err != nil {
		a.fleetProblem(w, r, err)
		return
	}
	setETag(w, plan.DesiredRevision)
	a.json(w, http.StatusOK, planResponse(plan))
}

func (a *API) invalidPlanRequest(w http.ResponseWriter, r *http.Request) {
	if err := a.control.RecordDenied(r.Context(), "PLAN_REQUEST", "PLAN", "INVALID_REQUEST", requestMeta(r)); err != nil {
		a.internalProblem(w, r)
		return
	}
	a.problem(w, r, http.StatusBadRequest, "INVALID_REQUEST", "Invalid request", "The plan request is invalid.", nil)
}

func (a *API) PausePlan(w http.ResponseWriter, r *http.Request, planID openapi_types.UUID, params PausePlanParams) {
	a.planTransition(w, r, planID, params.XCSRFToken, params.IfMatch, "PLAN_PAUSE",
		func(revision int64, actor domain.User) (domain.Plan, error) {
			return a.control.PausePlan(r.Context(), planID, revision, actor, requestMeta(r))
		})
}

func (a *API) ResumePlan(w http.ResponseWriter, r *http.Request, planID openapi_types.UUID, params ResumePlanParams) {
	a.planTransition(w, r, planID, params.XCSRFToken, params.IfMatch, "PLAN_RESUME",
		func(revision int64, actor domain.User) (domain.Plan, error) {
			return a.control.ResumePlan(r.Context(), planID, revision, actor, requestMeta(r))
		})
}

func (a *API) ApplyPlan(w http.ResponseWriter, r *http.Request, planID openapi_types.UUID, params ApplyPlanParams) {
	a.planTransition(w, r, planID, params.XCSRFToken, params.IfMatch, "PLAN_APPLY",
		func(revision int64, actor domain.User) (domain.Plan, error) {
			return a.control.ApplyPlan(r.Context(), planID, revision, actor, requestMeta(r))
		})
}

func (a *API) planTransition(w http.ResponseWriter, r *http.Request, planID openapi_types.UUID, csrf CsrfToken, ifMatch IfMatch,
	action string, transition func(int64, domain.User) (domain.Plan, error)) {
	actor, ok := a.authorizeMutation(w, r, csrf, action, "PLAN")
	if !ok {
		return
	}
	if r.URL.RawQuery != "" {
		a.invalidPlanRequest(w, r)
		return
	}
	revision, err := parseETag(ifMatch)
	if err != nil {
		a.problem(w, r, http.StatusBadRequest, "INVALID_REVISION", "Invalid request", "If-Match must contain a positive revision.", nil)
		return
	}
	plan, err := transition(revision, actor.User)
	if err != nil {
		a.fleetProblem(w, r, err)
		return
	}
	setETag(w, plan.DesiredRevision)
	a.json(w, http.StatusOK, planResponse(plan))
}

func (a *API) ApplyTemplateToPlans(w http.ResponseWriter, r *http.Request, templateID openapi_types.UUID, params ApplyTemplateToPlansParams) {
	actor, ok := a.authorizeMutation(w, r, params.XCSRFToken, "TEMPLATE_APPLY_TO_PLANS", "TEMPLATE")
	if !ok {
		return
	}
	if r.URL.RawQuery != "" {
		a.invalidTemplateRequest(w, r)
		return
	}
	var request TemplateApplyRequest
	if err := decodeJSON(w, r, &request); err != nil {
		a.invalidTemplateRequest(w, r)
		return
	}
	targets := make([]domain.PlanApplyTarget, 0, len(request.Plans))
	for _, item := range request.Plans {
		targets = append(targets, domain.PlanApplyTarget{PlanID: item.PlanId, ExpectedRevision: item.ExpectedRevision})
	}
	results, err := a.control.ApplyTemplateToPlans(r.Context(), templateID, request.TemplateRevision, targets, actor.User, requestMeta(r))
	if err != nil {
		a.fleetProblem(w, r, err)
		return
	}
	response := TemplateApplyResponse{Results: make([]PlanApplyResult, 0, len(results))}
	for _, result := range results {
		item := PlanApplyResult{PlanId: result.PlanID, Status: PlanApplyResultStatus(result.Status)}
		if result.Code != "" {
			code := result.Code
			item.Code = &code
		}
		response.Results = append(response.Results, item)
	}
	a.json(w, http.StatusOK, response)
}
