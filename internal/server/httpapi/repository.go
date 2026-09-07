package httpapi

import (
	"encoding/base64"
	"net/http"

	"github.com/google/uuid"
	"github.com/sagehou/restfleet/internal/domain"
)

func repositoryResponse(r domain.Repository) Repository {
	return Repository{Id: r.ID, Name: r.Name, HostId: r.HostID, StorageCredentialId: r.StorageCredentialID,
		Status: RepositoryStatus(r.Status), FormatVersion: r.FormatVersion,
		GatewaySecretRevision: r.GatewaySecretRevision, ResticSecretRevision: r.ResticSecretRevision,
		Revision: r.Revision, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt}
}

func (a *API) ListRepositories(w http.ResponseWriter, r *http.Request, params ListRepositoriesParams) {
	if _, ok := a.authorizeRead(w, r); !ok {
		return
	}
	after, limit, ok := a.listPage(w, r, params.Limit, params.Cursor)
	if !ok {
		return
	}
	items, err := a.control.Repositories(r.Context(), after, limit+1)
	if err != nil {
		a.fleetProblem(w, r, err)
		return
	}
	response := RepositoryList{Items: make([]Repository, 0)}
	if len(items) > limit {
		last := items[limit-1].ID
		cursor := base64.RawURLEncoding.EncodeToString(last[:])
		response.NextCursor = &cursor
		items = items[:limit]
	}
	for _, repo := range items {
		response.Items = append(response.Items, repositoryResponse(repo))
	}
	a.json(w, http.StatusOK, response)
}

func (a *API) GetRepository(w http.ResponseWriter, r *http.Request, id uuid.UUID) {
	if _, ok := a.authorizeRead(w, r); !ok {
		return
	}
	if r.URL.RawQuery != "" {
		a.invalidRepositoryRequest(w, r)
		return
	}
	repo, err := a.control.Repository(r.Context(), id)
	if err != nil {
		a.fleetProblem(w, r, err)
		return
	}
	setETag(w, repo.Revision)
	a.json(w, http.StatusOK, repositoryResponse(repo))
}

func (a *API) CreateRepository(w http.ResponseWriter, r *http.Request, params CreateRepositoryParams) {
	actor, ok := a.authorizeMutation(w, r, params.XCSRFToken, "REPOSITORY_CREATE", "REPOSITORY")
	if !ok {
		return
	}
	var request RepositoryCreate
	if r.URL.RawQuery != "" {
		a.invalidRepositoryRequest(w, r)
		return
	}
	if err := decodeJSON(w, r, &request); err != nil {
		a.invalidRepositoryRequest(w, r)
		return
	}
	shared := request.Shared != nil && *request.Shared
	repo, err := a.control.CreateRepository(r.Context(), request.Name, request.HostId, request.StorageCredentialId, shared, actor.User, requestMeta(r))
	if err != nil {
		a.fleetProblem(w, r, err)
		return
	}
	setETag(w, repo.Revision)
	w.Header().Set("Location", "/api/v1/repositories/"+repo.ID.String())
	a.json(w, http.StatusCreated, repositoryResponse(repo))
}

func (a *API) invalidRepositoryRequest(w http.ResponseWriter, r *http.Request) {
	if err := a.control.RecordDenied(r.Context(), "REPOSITORY_REQUEST", "REPOSITORY", "INVALID_REQUEST", requestMeta(r)); err != nil {
		a.internalProblem(w, r)
		return
	}
	a.problem(w, r, http.StatusBadRequest, "INVALID_REQUEST", "Invalid request", "The repository request is invalid.", nil)
}
