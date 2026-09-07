package httpapi

import (
	"io"
	"net/http"

	"github.com/google/uuid"
)

func (a *API) InitializeRepository(w http.ResponseWriter, r *http.Request, id uuid.UUID, params InitializeRepositoryParams) {
	actor, ok := a.authorizeMutation(w, r, params.XCSRFToken, "REPOSITORY_INITIALIZE", "REPOSITORY")
	if !ok {
		return
	}
	if r.URL.RawQuery != "" || len(r.Header.Values("Idempotency-Key")) != 1 {
		a.invalidRepositoryRequest(w, r)
		return
	}
	if r.Body != nil {
		body, err := io.ReadAll(io.LimitReader(r.Body, 1))
		if err != nil || len(body) != 0 {
			a.invalidRepositoryRequest(w, r)
			return
		}
	}
	o, err := a.control.InitializeRepository(r.Context(), id, params.IdempotencyKey, actor.User, requestMeta(r))
	if err != nil {
		a.fleetProblem(w, r, err)
		return
	}
	w.Header().Set("Location", "/api/v1/operations/"+o.ID.String())
	a.json(w, http.StatusAccepted, operationResponse(o))
}
