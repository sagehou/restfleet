package httpapi

import (
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sagehou/restfleet/internal/domain"
	"github.com/sagehou/restfleet/internal/security"
	control "github.com/sagehou/restfleet/internal/server"
)

func (a *API) ListHosts(w http.ResponseWriter, r *http.Request) {
	authenticated, ok := a.authorizeRead(w, r)
	if !ok {
		return
	}
	_ = authenticated
	hosts, err := a.control.Hosts(r.Context())
	if err != nil {
		a.internalProblem(w, r)
		return
	}
	items := make([]Host, 0, len(hosts))
	for _, host := range hosts {
		items = append(items, hostResponse(host))
	}
	a.json(w, http.StatusOK, HostList{Items: items})
}

func (a *API) CreateHost(w http.ResponseWriter, r *http.Request, params CreateHostParams) {
	authenticated, ok := a.authorizeMutation(w, r, params.XCSRFToken, "HOST_CREATE", "HOST")
	if !ok {
		return
	}
	var request HostCreate
	if err := decodeJSON(w, r, &request); err != nil {
		a.problem(w, r, http.StatusBadRequest, "INVALID_REQUEST", "Invalid request", "The request body is invalid.", nil)
		return
	}
	description := ""
	if request.Description != nil {
		description = *request.Description
	}
	labels := map[string]string{}
	if request.Labels != nil {
		labels = *request.Labels
	}
	host, err := a.control.CreateHost(
		r.Context(), request.DisplayName, description, request.Timezone, labels,
		authenticated.User, requestMeta(r),
	)
	if err != nil {
		a.fleetProblem(w, r, err)
		return
	}
	setETag(w, host.Revision)
	a.json(w, http.StatusCreated, hostResponse(host))
}

func (a *API) GetHost(w http.ResponseWriter, r *http.Request, hostID HostId) {
	if _, ok := a.authorizeRead(w, r); !ok {
		return
	}
	host, err := a.control.Host(r.Context(), hostID)
	if err != nil {
		a.fleetProblem(w, r, err)
		return
	}
	setETag(w, host.Revision)
	a.json(w, http.StatusOK, hostResponse(host))
}

func (a *API) UpdateHost(
	w http.ResponseWriter,
	r *http.Request,
	hostID HostId,
	params UpdateHostParams,
) {
	authenticated, ok := a.authorizeMutation(w, r, params.XCSRFToken, "HOST_UPDATE", "HOST")
	if !ok {
		return
	}
	revision, err := parseETag(params.IfMatch)
	if err != nil {
		a.problem(w, r, http.StatusBadRequest, "INVALID_REVISION", "Invalid request", "If-Match must contain a positive revision.", nil)
		return
	}
	var request HostPatch
	if err := decodeJSON(w, r, &request); err != nil {
		a.problem(w, r, http.StatusBadRequest, "INVALID_REQUEST", "Invalid request", "The request body is invalid.", nil)
		return
	}
	host, err := a.control.UpdateHost(r.Context(), hostID, revision, control.HostPatch{
		DisplayName: request.DisplayName,
		Description: request.Description,
		Labels:      request.Labels,
		Timezone:    request.Timezone,
	}, authenticated.User, requestMeta(r))
	if err != nil {
		a.fleetProblem(w, r, err)
		return
	}
	setETag(w, host.Revision)
	a.json(w, http.StatusOK, hostResponse(host))
}

func (a *API) DisableHost(w http.ResponseWriter, r *http.Request, hostID HostId, params DisableHostParams) {
	a.setHostEnabled(w, r, hostID, params.XCSRFToken, params.IfMatch, false)
}

func (a *API) EnableHost(w http.ResponseWriter, r *http.Request, hostID HostId, params EnableHostParams) {
	a.setHostEnabled(w, r, hostID, params.XCSRFToken, params.IfMatch, true)
}

func (a *API) setHostEnabled(
	w http.ResponseWriter,
	r *http.Request,
	hostID HostId,
	csrf, etag string,
	enabled bool,
) {
	authenticated, ok := a.authorizeMutation(w, r, csrf, "HOST_STATUS_CHANGE", "HOST")
	if !ok {
		return
	}
	revision, err := parseETag(etag)
	if err != nil {
		a.problem(w, r, http.StatusBadRequest, "INVALID_REVISION", "Invalid request", "If-Match must contain a positive revision.", nil)
		return
	}
	host, err := a.control.SetHostEnabled(
		r.Context(), hostID, revision, enabled, authenticated.User, requestMeta(r),
	)
	if err != nil {
		a.fleetProblem(w, r, err)
		return
	}
	setETag(w, host.Revision)
	a.json(w, http.StatusOK, hostResponse(host))
}

func (a *API) ListHostAgents(w http.ResponseWriter, r *http.Request, hostID HostId) {
	if _, ok := a.authorizeRead(w, r); !ok {
		return
	}
	agents, err := a.control.AgentsForHost(r.Context(), hostID)
	if err != nil {
		a.fleetProblem(w, r, err)
		return
	}
	items := make([]Agent, 0, len(agents))
	for _, agent := range agents {
		items = append(items, agentResponse(agent))
	}
	a.json(w, http.StatusOK, AgentList{Items: items})
}

func (a *API) GetHostInventory(w http.ResponseWriter, r *http.Request, hostID HostId) {
	if _, ok := a.authorizeRead(w, r); !ok {
		return
	}
	inventory, err := a.control.LatestAgentInventory(r.Context(), hostID)
	if err != nil {
		a.fleetProblem(w, r, err)
		return
	}
	a.json(w, http.StatusOK, agentInventoryResponse(inventory))
}

func (a *API) GetAgent(w http.ResponseWriter, r *http.Request, agentID AgentId) {
	if _, ok := a.authorizeRead(w, r); !ok {
		return
	}
	agent, err := a.control.Agent(r.Context(), agentID)
	if err != nil {
		a.fleetProblem(w, r, err)
		return
	}
	a.json(w, http.StatusOK, agentResponse(agent))
}

func (a *API) CreateEnrollmentToken(
	w http.ResponseWriter,
	r *http.Request,
	hostID HostId,
	params CreateEnrollmentTokenParams,
) {
	authenticated, ok := a.authorizeMutation(
		w, r, params.XCSRFToken, "ENROLLMENT_TOKEN_CREATE", "ENROLLMENT_TOKEN",
	)
	if !ok {
		return
	}
	var request EnrollmentTokenCreate
	if err := decodeJSON(w, r, &request); err != nil {
		a.problem(w, r, http.StatusBadRequest, "INVALID_REQUEST", "Invalid request", "The request body is invalid.", nil)
		return
	}
	ttl := 600
	if request.ExpiresInSeconds != nil {
		ttl = *request.ExpiresInSeconds
	}
	created, err := a.control.CreateEnrollmentToken(
		r.Context(), hostID, ttl, authenticated.User, requestMeta(r),
	)
	if err != nil {
		a.fleetProblem(w, r, err)
		return
	}
	a.json(w, http.StatusCreated, EnrollmentTokenCreated{
		Id:          created.Token.ID,
		Token:       created.Secret,
		Fingerprint: created.Token.Fingerprint,
		ExpiresAt:   created.Token.ExpiresAt,
		Install:     EnrollmentInstall{Native: created.Native, Docker: created.Docker},
	})
}

func (a *API) ListEnrollmentTokens(w http.ResponseWriter, r *http.Request, hostID HostId) {
	if _, ok := a.authorizeRead(w, r); !ok {
		return
	}
	tokens, err := a.control.EnrollmentTokens(r.Context(), hostID)
	if err != nil {
		a.fleetProblem(w, r, err)
		return
	}
	now := time.Now().UTC()
	items := make([]EnrollmentToken, 0, len(tokens))
	for _, token := range tokens {
		items = append(items, enrollmentTokenResponse(token, now))
	}
	a.json(w, http.StatusOK, EnrollmentTokenList{Items: items})
}

func (a *API) RevokeEnrollmentToken(
	w http.ResponseWriter,
	r *http.Request,
	tokenID TokenId,
	params RevokeEnrollmentTokenParams,
) {
	authenticated, ok := a.authorizeMutation(
		w, r, params.XCSRFToken, "ENROLLMENT_TOKEN_REVOKE", "ENROLLMENT_TOKEN",
	)
	if !ok {
		return
	}
	if err := a.control.RevokeEnrollmentToken(
		r.Context(), tokenID, authenticated.User, requestMeta(r),
	); err != nil {
		a.fleetProblem(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) EnrollAgent(w http.ResponseWriter, r *http.Request) {
	var request AgentEnrollmentRequest
	if err := decodeJSON(w, r, &request); err != nil {
		if err := a.control.RecordDenied(r.Context(), "AGENT_ENROLL", "AGENT", "INVALID_REQUEST", requestMeta(r)); err != nil {
			a.internalProblem(w, r)
			return
		}
		a.problem(w, r, http.StatusBadRequest, "INVALID_REQUEST", "Invalid request", "The request body is invalid.", nil)
		return
	}
	meta := requestMeta(r)
	ipKey := "ip:" + hex.EncodeToString(meta.SourceIPHash)
	tokenKey := "token:" + hex.EncodeToString(security.HashSecret(request.Token))
	if !a.enrollmentLimiter.allow(ipKey, tokenKey) {
		if err := a.control.RecordDenied(r.Context(), "AGENT_ENROLL", "AGENT", "RATE_LIMITED", meta); err != nil {
			a.internalProblem(w, r)
			return
		}
		a.problem(w, r, http.StatusTooManyRequests, "RATE_LIMITED", "Too many requests", "Try again later.", nil)
		return
	}
	enrolled, err := a.control.EnrollAgent(r.Context(), control.EnrollmentRequest{
		Token: request.Token, CSRPEM: request.CsrPem,
		InstallID: request.InstallId.String(), AgentVersion: request.AgentVersion,
		ProtocolVersion: request.ProtocolVersion, Hostname: request.Hostname,
		OS: string(request.Os), Arch: string(request.Arch),
		Capabilities: request.Capabilities,
	}, meta)
	if err != nil {
		a.fleetProblem(w, r, err)
		return
	}
	a.json(w, http.StatusCreated, AgentEnrollmentResponse{
		AgentId: enrolled.AgentID, HostId: enrolled.HostID,
		CertificatePem: enrolled.CertificatePEM, CaBundlePem: enrolled.CABundlePEM,
		NotAfter: enrolled.NotAfter, ServerName: enrolled.ServerName,
		GrpcEndpoint:             enrolled.GRPCEndpoint,
		HeartbeatIntervalSeconds: enrolled.HeartbeatSeconds,
	})
}

func (a *API) RevokeAgent(
	w http.ResponseWriter,
	r *http.Request,
	agentID AgentId,
	params RevokeAgentParams,
) {
	authenticated, ok := a.authorizeMutation(w, r, params.XCSRFToken, "AGENT_REVOKE", "AGENT")
	if !ok {
		return
	}
	if len(params.IdempotencyKey) < 8 || len(params.IdempotencyKey) > 128 ||
		strings.TrimSpace(params.IdempotencyKey) != params.IdempotencyKey {
		a.problem(w, r, http.StatusBadRequest, "INVALID_IDEMPOTENCY_KEY", "Invalid request", "Idempotency-Key is invalid.", nil)
		return
	}
	var request AgentRevoke
	if err := decodeJSON(w, r, &request); err != nil {
		a.problem(w, r, http.StatusBadRequest, "INVALID_REQUEST", "Invalid request", "The request body is invalid.", nil)
		return
	}
	agent, err := a.control.RevokeAgent(
		r.Context(), agentID, request.Reason, authenticated.User, requestMeta(r),
	)
	if err != nil {
		a.fleetProblem(w, r, err)
		return
	}
	a.json(w, http.StatusOK, agentResponse(agent))
}

func (a *API) authorizeRead(
	w http.ResponseWriter,
	r *http.Request,
) (domain.AuthenticatedSession, bool) {
	authenticated, ok := a.authenticate(w, r)
	if !ok {
		return domain.AuthenticatedSession{}, false
	}
	if !a.readLimiter.allow("session:" + authenticated.Session.ID.String()) {
		a.problem(w, r, http.StatusTooManyRequests, "RATE_LIMITED", "Too many requests", "Try again later.", nil)
		return domain.AuthenticatedSession{}, false
	}
	return authenticated, true
}

func (a *API) authorizeMutation(
	w http.ResponseWriter,
	r *http.Request,
	csrf, action, resourceType string,
) (domain.AuthenticatedSession, bool) {
	authenticated, ok := a.authenticate(w, r)
	if !ok {
		return domain.AuthenticatedSession{}, false
	}
	meta := requestMeta(r)
	if !a.mutationLimiter.allow("session:" + authenticated.Session.ID.String()) {
		if err := a.control.RecordDenied(r.Context(), action, resourceType, "RATE_LIMITED", meta); err != nil {
			a.internalProblem(w, r)
			return domain.AuthenticatedSession{}, false
		}
		a.problem(w, r, http.StatusTooManyRequests, "RATE_LIMITED", "Too many requests", "Try again later.", nil)
		return domain.AuthenticatedSession{}, false
	}
	if csrf == "" || !security.SecretHashMatches(authenticated.Session.CSRFSecretHash, csrf) {
		if err := a.control.RecordDenied(r.Context(), action, resourceType, "CSRF_INVALID", meta); err != nil {
			a.internalProblem(w, r)
			return domain.AuthenticatedSession{}, false
		}
		a.problem(w, r, http.StatusForbidden, "CSRF_INVALID", "Request denied", "The CSRF token is missing or invalid.", nil)
		return domain.AuthenticatedSession{}, false
	}
	if authenticated.User.Role != domain.RoleAdmin {
		if err := a.control.RecordDenied(r.Context(), action, resourceType, "ROLE_DENIED", meta); err != nil {
			a.internalProblem(w, r)
			return domain.AuthenticatedSession{}, false
		}
		a.problem(w, r, http.StatusForbidden, "ROLE_DENIED", "Request denied", "Administrator access is required.", nil)
		return domain.AuthenticatedSession{}, false
	}
	return authenticated, true
}

func hostResponse(host domain.Host) Host {
	return Host{
		Id: host.ID, DisplayName: host.DisplayName, Description: host.Description,
		Labels: cloneMap(host.Labels), Timezone: host.Timezone,
		Status: HostStatus(host.Status), Revision: host.Revision,
		CreatedAt: host.CreatedAt, UpdatedAt: host.UpdatedAt,
	}
}

func agentResponse(agent domain.Agent) Agent {
	response := Agent{
		Id: agent.ID, HostId: agent.HostID, InstallId: agent.InstallID,
		PublicKeyFingerprint: agent.PublicKeyFingerprint,
		CertificateSerial:    agent.CertificateSerial,
		CertificateNotAfter:  agent.CertificateNotAfter,
		Status:               AgentStatus(agent.Status), Health: AgentHealth(agent.Health),
		Version: agent.Version, ProtocolVersion: agent.ProtocolVersion,
		Os: agent.OS, Arch: agent.Arch, Hostname: agent.Hostname,
		DesiredRevision: agent.DesiredRevision, AcceptedRevision: agent.AcceptedRevision,
		UptimeSeconds: agent.UptimeSeconds, StateFreeBytes: agent.StateFreeBytes,
		ClockOffsetMs:      agent.ClockOffsetMS,
		HeartbeatErrorCode: agent.HeartbeatErrorCode,
		ConfigErrorCode:    agent.ConfigErrorCode, ConfigErrorField: agent.ConfigErrorField,
		CreatedAt: agent.CreatedAt, UpdatedAt: agent.UpdatedAt,
		LastSeenAt: agent.LastSeenAt, LastConnectedAt: agent.LastConnectedAt,
	}
	if agent.BootID != "" {
		response.BootId = &agent.BootID
	}
	if agent.ResticVersion != "" {
		response.ResticVersion = &agent.ResticVersion
	}
	return response
}

func agentInventoryResponse(inventory domain.AgentInventory) AgentInventory {
	available := make(map[string]int64, len(inventory.AvailableBytes))
	for name, value := range inventory.AvailableBytes {
		available[name] = int64(value)
	}
	return AgentInventory{
		Id: inventory.ID, AgentId: inventory.AgentID, CapturedAt: inventory.CapturedAt,
		Kernel: inventory.Kernel, OsRelease: inventory.OSRelease,
		CpuArch:      AgentInventoryCpuArch(inventory.CPUArch),
		AgentVersion: inventory.AgentVersion, ResticVersion: inventory.ResticVersion,
		Containerized: inventory.Containerized, AvailableBytes: available,
		ClockOffsetMs: inventory.ClockOffsetMS,
		Capabilities:  append([]string(nil), inventory.Capabilities...),
	}
}

func enrollmentTokenResponse(token domain.EnrollmentToken, now time.Time) EnrollmentToken {
	return EnrollmentToken{
		Id: token.ID, HostId: token.HostID, Fingerprint: token.Fingerprint,
		Status: EnrollmentTokenStatus(token.Status(now)), ExpiresAt: token.ExpiresAt,
		CreatedAt: token.CreatedAt, UsedAt: token.UsedAt,
		UsedByAgentId: token.UsedByAgentID, RevokedAt: token.RevokedAt,
	}
}

func cloneMap(values map[string]string) map[string]string {
	result := make(map[string]string, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}

func setETag(w http.ResponseWriter, revision int64) {
	w.Header().Set("ETag", strconv.Quote(strconv.FormatInt(revision, 10)))
}

func parseETag(value string) (int64, error) {
	trimmed := strings.Trim(strings.TrimSpace(value), "\"")
	revision, err := strconv.ParseInt(trimmed, 10, 64)
	if err != nil || revision < 1 {
		return 0, control.ErrInvalidRevision
	}
	return revision, nil
}

func (a *API) fleetProblem(w http.ResponseWriter, r *http.Request, err error) {
	var validation *control.ValidationError
	switch {
	case errors.As(err, &validation):
		fieldErrors := []FieldError{{Field: validation.Field, Code: validation.Code}}
		a.problem(w, r, http.StatusUnprocessableEntity, "VALIDATION_FAILED", "Validation failed", "One or more fields are invalid.", &fieldErrors)
	case errors.Is(err, control.ErrForbidden):
		a.problem(w, r, http.StatusForbidden, "ROLE_DENIED", "Request denied", "Administrator access is required.", nil)
	case errors.Is(err, domain.ErrStorageUnavailable):
		a.problem(w, r, http.StatusServiceUnavailable, "STORAGE_UNAVAILABLE", "Storage unavailable", "Storage credential management is not available.", nil)
	case errors.Is(err, domain.ErrCredentialTestBusy):
		a.problem(w, r, http.StatusConflict, "CREDENTIAL_TEST_BUSY", "Conflict", "A credential test is already active.", nil)
	case errors.Is(err, domain.ErrIdempotencyReused):
		a.problem(w, r, http.StatusConflict, "IDEMPOTENCY_KEY_REUSED", "Conflict", "The idempotency key was used for another request.", nil)
	case errors.Is(err, domain.ErrCredentialDisabled):
		a.problem(w, r, http.StatusConflict, "CREDENTIAL_DISABLED", "Credential disabled", "Disabled credentials cannot be replaced.", nil)
	case errors.Is(err, domain.ErrStorageTargetChanged):
		a.problem(w, r, http.StatusConflict, "STORAGE_TARGET_CHANGED", "Storage target changed", "Replacement must preserve the storage target and Crypt settings.", nil)
	case errors.Is(err, domain.ErrNotFound):
		a.problem(w, r, http.StatusNotFound, "NOT_FOUND", "Not found", "The requested resource was not found.", nil)
	case errors.Is(err, domain.ErrAlreadyExists):
		a.problem(w, r, http.StatusConflict, "ALREADY_EXISTS", "Conflict", "A resource with the same identity already exists.", nil)
	case errors.Is(err, domain.ErrRevisionConflict):
		a.problem(w, r, http.StatusPreconditionFailed, "REVISION_CONFLICT", "Revision conflict", "The resource changed; reload it and try again.", nil)
	case errors.Is(err, domain.ErrEnrollmentTokenInvalid):
		a.problem(w, r, http.StatusForbidden, "ENROLLMENT_DENIED", "Enrollment denied", "The enrollment request was denied.", nil)
	case errors.Is(err, control.ErrInvalidEnrollment):
		a.problem(w, r, http.StatusUnprocessableEntity, "INVALID_ENROLLMENT", "Invalid enrollment", "The enrollment request is invalid.", nil)
	case errors.Is(err, control.ErrIncompatibleAgent):
		a.problem(w, r, http.StatusConflict, "PROTOCOL_INCOMPATIBLE", "Incompatible Agent", "The Agent protocol version is not supported.", nil)
	case errors.Is(err, domain.ErrAgentRevoked):
		a.problem(w, r, http.StatusForbidden, "AGENT_REVOKED", "Agent revoked", "The Agent identity is no longer active.", nil)
	case errors.Is(err, domain.ErrEnrollmentUnavailable):
		a.problem(w, r, http.StatusServiceUnavailable, "ENROLLMENT_UNAVAILABLE", "Enrollment unavailable", "Agent enrollment is not configured.", nil)
	default:
		a.internalProblem(w, r)
	}
}
