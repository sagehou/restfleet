package httpapi

import (
	"encoding/base64"
	"net/http"

	openapi_types "github.com/oapi-codegen/runtime/types"
	"github.com/sagehou/restfleet/internal/domain"
)

func backupOperationResponse(o domain.BackupOperation) BackupOperation {
	filesDone, totalFiles, bytesDone, totalBytes := o.FilesDone, o.TotalFiles, o.BytesDone, o.TotalBytes
	return BackupOperation{
		Id: o.ID, PlanId: o.PlanID, AgentId: o.AgentID, Type: BackupOperationType(o.Type),
		Source: BackupOperationSource(o.Source), Status: BackupOperationStatus(o.Status),
		PlanRevision: o.PlanRevision, ScheduledAt: o.ScheduledAt, StartedAt: o.StartedAt,
		FinishedAt: o.FinishedAt, ExitCode: o.ExitCode, ErrorCode: &o.ErrorCode, SnapshotId: &o.SnapshotID,
		FilesDone: &filesDone, TotalFiles: &totalFiles, BytesDone: &bytesDone, TotalBytes: &totalBytes,
		CreatedAt: o.CreatedAt, UpdatedAt: o.UpdatedAt,
	}
}

func (a *API) ListBackupOperations(w http.ResponseWriter, r *http.Request, params ListBackupOperationsParams) {
	if _, ok := a.authorizeRead(w, r); !ok {
		return
	}
	after, limit, ok := a.listPage(w, r, params.Limit, params.Cursor)
	if !ok {
		return
	}
	items, err := a.control.BackupOperations(r.Context(), after, limit+1)
	if err != nil {
		a.fleetProblem(w, r, err)
		return
	}
	response := BackupOperationList{Items: make([]BackupOperation, 0)}
	if len(items) > limit {
		last := items[limit-1].ID
		cursor := base64.RawURLEncoding.EncodeToString(last[:])
		response.NextCursor = &cursor
		items = items[:limit]
	}
	for _, item := range items {
		response.Items = append(response.Items, backupOperationResponse(item))
	}
	a.json(w, http.StatusOK, response)
}

func (a *API) GetBackupOperation(w http.ResponseWriter, r *http.Request, operationID openapi_types.UUID) {
	if _, ok := a.authorizeRead(w, r); !ok {
		return
	}
	if r.URL.RawQuery != "" {
		a.problem(w, r, http.StatusBadRequest, "INVALID_REQUEST", "Invalid request", "The request is invalid.", nil)
		return
	}
	operation, err := a.control.BackupOperation(r.Context(), operationID)
	if err != nil {
		a.fleetProblem(w, r, err)
		return
	}
	a.json(w, http.StatusOK, backupOperationResponse(operation))
}
