package httpapi

import (
	"encoding/hex"
	"encoding/json"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/sagehou/restfleet/internal/domain"
)

func offlineAuthorizationResponse(a domain.OfflineAuthorization) OfflineAuthorization {
	return OfflineAuthorization{
		Id:                    a.ID,
		Owner:                 a.Owner,
		GatewayInstanceId:     a.GatewayInstanceID,
		AgentId:               a.AgentID,
		HostId:                a.HostID,
		RepositoryId:          a.RepositoryID,
		GatewayId:             a.GatewayID,
		StorageCredentialId:   a.StorageCredentialID,
		DeliveryId:            a.DeliveryID,
		ConfigurationHash:     a.ConfigurationHash,
		AuthorizedAt:          a.AuthorizedAt,
		ExpiresAt:             a.ExpiresAt,
		AuthorizationSequence: a.AuthorizationSequence,
		RenewedAt:             a.RenewedAt,
		RevokedAt:             a.RevokedAt,
		DisabledAt:            a.DisabledAt,
		CreatedAt:             a.CreatedAt,
	}
}

func writebackRecordResponse(r domain.WritebackRecord) WritebackRecord {
	return WritebackRecord{
		Id:                r.ID,
		AuthorizationId:   r.AuthorizationID,
		Owner:             r.Owner,
		GatewayInstanceId: r.GatewayInstanceID,
		Sequence:          r.Sequence,
		RecordType:        WritebackRecordRecordType(r.RecordType),
		CreatedAt:         r.CreatedAt,
		ConfirmedAt:       r.ConfirmedAt,
	}
}

func (a *API) IssueOfflineAuthorization(w http.ResponseWriter, r *http.Request, params IssueOfflineAuthorizationParams) {
	if _, ok := a.authorizeMutation(w, r, params.XCSRFToken, "OFFLINE_AUTH_ISSUE", "REPOSITORY"); !ok {
		return
	}
	var body IssueOfflineAuthorizationJSONBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		a.problem(w, r, http.StatusBadRequest, "INVALID_REQUEST", "Invalid request", "The request body is invalid JSON.", nil)
		return
	}
	if body.LifetimeHours < 1 || body.LifetimeHours > 12 {
		a.problem(w, r, http.StatusBadRequest, "INVALID_REQUEST", "Invalid request", "lifetime_hours must be between 1 and 12.", nil)
		return
	}
	lifetime := time.Duration(float64(body.LifetimeHours) * float64(time.Hour))
	result, err := a.control.IssueOfflineAuthorization(r.Context(), body.AgentId, body.GatewayInstanceId, lifetime)
	if err != nil {
		a.fleetProblem(w, r, err)
		return
	}
	a.json(w, http.StatusCreated, offlineAuthorizationResponse(result))
}

func (a *API) RenewOfflineAuthorization(w http.ResponseWriter, r *http.Request, authId uuid.UUID, params RenewOfflineAuthorizationParams) {
	if _, ok := a.authorizeMutation(w, r, params.XCSRFToken, "OFFLINE_AUTH_RENEW", "REPOSITORY"); !ok {
		return
	}
	var body RenewOfflineAuthorizationJSONBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		a.problem(w, r, http.StatusBadRequest, "INVALID_REQUEST", "Invalid request", "The request body is invalid JSON.", nil)
		return
	}
	if body.NewLifetimeHours < 1 || body.NewLifetimeHours > 12 {
		a.problem(w, r, http.StatusBadRequest, "INVALID_REQUEST", "Invalid request", "new_lifetime_hours must be between 1 and 12.", nil)
		return
	}
	lifetime := time.Duration(float64(body.NewLifetimeHours) * float64(time.Hour))
	result, err := a.control.RenewOfflineAuthorization(r.Context(), authId, body.Owner, int64(body.CurrentSequence), lifetime)
	if err != nil {
		a.fleetProblem(w, r, err)
		return
	}
	a.json(w, http.StatusOK, offlineAuthorizationResponse(result))
}

func (a *API) RevokeOfflineAuthorization(w http.ResponseWriter, r *http.Request, authId uuid.UUID, params RevokeOfflineAuthorizationParams) {
	if _, ok := a.authorizeMutation(w, r, params.XCSRFToken, "OFFLINE_AUTH_REVOKE", "REPOSITORY"); !ok {
		return
	}
	var body RevokeOfflineAuthorizationJSONBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		a.problem(w, r, http.StatusBadRequest, "INVALID_REQUEST", "Invalid request", "The request body is invalid JSON.", nil)
		return
	}
	if err := a.control.RevokeOfflineAuthorization(r.Context(), authId, body.Owner); err != nil {
		a.fleetProblem(w, r, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (a *API) SubmitWritebackRecord(w http.ResponseWriter, r *http.Request) {
	// Writeback is a Gateway operation via mTLS, exempt from CSRF.
	if _, ok := a.authenticate(w, r); !ok {
		return
	}
	var body SubmitWritebackRecordJSONBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		a.problem(w, r, http.StatusBadRequest, "INVALID_REQUEST", "Invalid request", "The request body is invalid JSON.", nil)
		return
	}

	checksum, err := hex.DecodeString(body.ChecksumHex)
	if err != nil || len(checksum) != 32 {
		a.problem(w, r, http.StatusBadRequest, "INVALID_CHECKSUM", "Invalid checksum", "checksum_hex must be 64 hex characters (32 bytes).", nil)
		return
	}

	submission := domain.WritebackSubmission{
		RecordID:          body.RecordId,
		AuthorizationID:   body.AuthorizationId,
		Owner:             body.Owner,
		GatewayInstanceID: body.GatewayInstanceId,
		Sequence:          int64(body.Sequence),
		RecordType:        string(body.RecordType),
		Payload:           body.PayloadBase64,
		Checksum:          checksum,
		CreatedAt:         time.Now().UTC(),
	}

	result, err := a.control.SubmitWritebackRecord(r.Context(), submission)
	if err != nil {
		a.fleetProblem(w, r, err)
		return
	}
	a.json(w, http.StatusCreated, writebackRecordResponse(result))
}
