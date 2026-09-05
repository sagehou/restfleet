package httpapi

import (
	"encoding/base64"
	"net/http"

	"github.com/google/uuid"

	"github.com/sagehou/restfleet/internal/domain"
)

func storageCredentialResponse(c domain.StorageCredential) StorageCredential {
	return StorageCredential{
		Id: c.ID, Name: c.Name, Provider: StorageCredentialProvider(c.Provider),
		RemoteName: c.RemoteName, Status: StorageCredentialStatus(c.Status),
		SecretRevision: c.SecretRevision, Revision: c.Revision,
		CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt,
		LastTestOperationId: c.LastTestOperationID, LastTestedAt: c.LastTestedAt,
		LastTestResult: &c.LastTestResult, LastRefreshedAt: c.LastRefreshedAt,
	}
}

func (a *API) ListStorageCredentials(w http.ResponseWriter, r *http.Request, params ListStorageCredentialsParams) {
	if _, ok := a.authorizeRead(w, r); !ok {
		return
	}
	for key, values := range r.URL.Query() {
		if (key != "limit" && key != "cursor") || len(values) != 1 {
			a.problem(w, r, http.StatusBadRequest, "INVALID_QUERY", "Invalid request", "The list query is invalid.", nil)
			return
		}
	}
	limit := 50
	if params.Limit != nil {
		limit = *params.Limit
	}
	after := uuid.Nil
	if params.Cursor != nil {
		raw, err := base64.RawURLEncoding.DecodeString(*params.Cursor)
		if err != nil || len(raw) != 16 {
			a.problem(w, r, http.StatusBadRequest, "INVALID_CURSOR", "Invalid request", "The cursor is invalid.", nil)
			return
		}
		copy(after[:], raw)
	}
	if limit < 1 || limit > 200 {
		a.problem(w, r, http.StatusBadRequest, "INVALID_LIMIT", "Invalid request", "Limit must be between 1 and 200.", nil)
		return
	}
	credentials, err := a.control.StorageCredentials(r.Context(), after, limit+1)
	if err != nil {
		a.fleetProblem(w, r, err)
		return
	}
	response := StorageCredentialList{Items: make([]StorageCredential, 0)}
	if len(credentials) > limit {
		last := credentials[limit-1].ID
		cursor := base64.RawURLEncoding.EncodeToString(last[:])
		response.NextCursor = &cursor
		credentials = credentials[:limit]
	}
	for _, c := range credentials {
		response.Items = append(response.Items, storageCredentialResponse(c))
	}
	a.json(w, http.StatusOK, response)
}

func (a *API) GetStorageCredential(w http.ResponseWriter, r *http.Request, id uuid.UUID) {
	if _, ok := a.authorizeRead(w, r); !ok {
		return
	}
	c, err := a.control.StorageCredential(r.Context(), id)
	if err != nil {
		a.fleetProblem(w, r, err)
		return
	}
	setETag(w, c.Revision)
	a.json(w, http.StatusOK, storageCredentialResponse(c))
}

func (a *API) CreateStorageCredential(w http.ResponseWriter, r *http.Request, params CreateStorageCredentialParams) {
	actor, ok := a.authorizeMutation(w, r, params.XCSRFToken, "STORAGE_CREDENTIAL_CREATE", "STORAGE_CREDENTIAL")
	if !ok {
		return
	}
	var request StorageCredentialCreate
	if err := decodeJSON(w, r, &request); err != nil || request.RcloneConfig == nil {
		a.invalidCredentialRequest(w, r)
		return
	}
	c, err := a.control.CreateStorageCredential(r.Context(), request.Name, request.RemoteName, *request.RcloneConfig, actor.User, requestMeta(r))
	request.RcloneConfig = nil
	if err != nil {
		a.fleetProblem(w, r, err)
		return
	}
	setETag(w, c.Revision)
	a.json(w, http.StatusCreated, storageCredentialResponse(c))
}

func (a *API) ReplaceStorageCredential(w http.ResponseWriter, r *http.Request, id uuid.UUID, params ReplaceStorageCredentialParams) {
	actor, ok := a.authorizeMutation(w, r, params.XCSRFToken, "STORAGE_CREDENTIAL_REPLACE", "STORAGE_CREDENTIAL")
	if !ok {
		return
	}
	revision, err := parseETag(params.IfMatch)
	if err != nil {
		a.invalidCredentialRequest(w, r)
		return
	}
	var request StorageCredentialReplace
	if err := decodeJSON(w, r, &request); err != nil || request.RcloneConfig == nil {
		a.invalidCredentialRequest(w, r)
		return
	}
	c, err := a.control.ReplaceStorageCredential(r.Context(), id, revision, *request.RcloneConfig, actor.User, requestMeta(r))
	request.RcloneConfig = nil
	if err != nil {
		a.fleetProblem(w, r, err)
		return
	}
	setETag(w, c.Revision)
	a.json(w, http.StatusOK, storageCredentialResponse(c))
}

func (a *API) DisableStorageCredential(w http.ResponseWriter, r *http.Request, id uuid.UUID, params DisableStorageCredentialParams) {
	actor, ok := a.authorizeMutation(w, r, params.XCSRFToken, "STORAGE_CREDENTIAL_DISABLE", "STORAGE_CREDENTIAL")
	if !ok {
		return
	}
	revision, err := parseETag(params.IfMatch)
	if err != nil {
		a.invalidCredentialRequest(w, r)
		return
	}
	c, err := a.control.DisableStorageCredential(r.Context(), id, revision, actor.User, requestMeta(r))
	if err != nil {
		a.fleetProblem(w, r, err)
		return
	}
	setETag(w, c.Revision)
	a.json(w, http.StatusOK, storageCredentialResponse(c))
}

func (a *API) invalidCredentialRequest(w http.ResponseWriter, r *http.Request) {
	if err := a.control.RecordDenied(r.Context(), "STORAGE_CREDENTIAL_CHANGE", "STORAGE_CREDENTIAL", "INVALID_REQUEST", requestMeta(r)); err != nil {
		a.internalProblem(w, r)
		return
	}
	a.problem(w, r, http.StatusBadRequest, "INVALID_REQUEST", "Invalid request", "The request body or revision is invalid.", nil)
}
