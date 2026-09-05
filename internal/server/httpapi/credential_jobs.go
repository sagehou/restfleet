package httpapi

import (
	"io"
	"net/http"

	"github.com/google/uuid"
	"github.com/sagehou/restfleet/internal/domain"
)

func operationResponse(o domain.Operation) Operation {
	return Operation{Id: o.ID, Type: OperationType(o.Type), Status: OperationStatus(o.Status), Source: OperationSource(o.Source),
		StorageCredentialId: o.StorageCredentialID, SecretRevision: o.SecretRevision, RequestedByUserId: o.RequestedByUserID,
		Attempt: o.Attempt, CreatedAt: o.CreatedAt, DispatchedAt: o.DispatchedAt, AcknowledgedAt: o.AcknowledgedAt,
		StartedAt: o.StartedAt, FinishedAt: o.FinishedAt, ErrorCode: OperationErrorCode(o.ErrorCode)}
}

func (a *API) TestStorageCredential(w http.ResponseWriter, r *http.Request, id uuid.UUID, params TestStorageCredentialParams) {
	actor, ok := a.authorizeMutation(w, r, params.XCSRFToken, "STORAGE_CREDENTIAL_TEST", "STORAGE_CREDENTIAL")
	if !ok {
		return
	}
	if r.URL.RawQuery != "" || len(r.Header.Values("Idempotency-Key")) != 1 {
		a.problem(w, r, http.StatusBadRequest, "INVALID_REQUEST", "Invalid request", "No query or duplicate idempotency header is allowed.", nil)
		return
	}
	if r.Body != nil {
		body, err := io.ReadAll(io.LimitReader(r.Body, 1))
		if err != nil || len(body) != 0 {
			a.problem(w, r, http.StatusBadRequest, "INVALID_REQUEST", "Invalid request", "This endpoint accepts no request body.", nil)
			return
		}
	}
	o, err := a.control.TestStorageCredential(r.Context(), id, params.IdempotencyKey, actor.User, requestMeta(r))
	if err != nil {
		a.fleetProblem(w, r, err)
		return
	}
	w.Header().Set("Location", "/api/v1/operations/"+o.ID.String())
	a.json(w, http.StatusAccepted, operationResponse(o))
}

func (a *API) GetOperation(w http.ResponseWriter, r *http.Request, id uuid.UUID) {
	if _, ok := a.authorizeRead(w, r); !ok {
		return
	}
	o, err := a.control.Operation(r.Context(), id)
	if err != nil {
		a.fleetProblem(w, r, err)
		return
	}
	a.json(w, http.StatusOK, operationResponse(o))
}
