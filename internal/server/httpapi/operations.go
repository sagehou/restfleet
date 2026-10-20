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

func snapshotResponse(s domain.Snapshot) Snapshot {
	return Snapshot{SnapshotId: s.SnapshotID, PlanId: s.PlanID, AgentId: s.AgentID, HostId: s.HostID,
		RepositoryId: s.RepositoryID, OperationId: s.OperationID, CapturedAt: s.CapturedAt,
		RecordedAt: s.RecordedAt, FilesNew: s.FilesNew, BytesAdded: s.BytesAdded}
}

func (a *API) ListSnapshots(w http.ResponseWriter, r *http.Request, params ListSnapshotsParams) {
	if _, ok := a.authorizeRead(w, r); !ok {
		return
	}
	limit := 100
	if params.Limit != nil {
		limit = *params.Limit
	}
	if r.URL.RawQuery != "" && params.Limit == nil {
		a.problem(w, r, http.StatusBadRequest, "INVALID_REQUEST", "Invalid request", "The request is invalid.", nil)
		return
	}
	items, err := a.control.Snapshots(r.Context(), limit)
	if err != nil {
		a.fleetProblem(w, r, err)
		return
	}
	response := SnapshotList{Items: make([]Snapshot, 0, len(items))}
	for _, item := range items {
		response.Items = append(response.Items, snapshotResponse(item))
	}
	a.json(w, http.StatusOK, response)
}

func (a *API) ListSnapshotEntries(w http.ResponseWriter, r *http.Request, snapshotID string, params ListSnapshotEntriesParams) {
	authenticated, ok := a.authorizeRead(w, r)
	if !ok {
		return
	}
	if r.URL.RawQuery != "" && params.Path == nil && params.RepositoryId == nil {
		a.problem(w, r, http.StatusBadRequest, "INVALID_REQUEST", "Invalid request", "The request is invalid.", nil)
		return
	}
	path := "/"
	if params.Path != nil {
		path = *params.Path
	}
	result, pending, err := a.control.SnapshotEntries(r.Context(), snapshotID, params.RepositoryId, path, authenticated.User.ID)
	if err != nil {
		a.fleetProblem(w, r, err)
		return
	}
	if pending {
		w.Header().Set("Retry-After", "1")
		a.json(w, http.StatusAccepted, SnapshotScanPending{
			Status: SnapshotScanPendingStatusPENDING, OperationId: result.OperationID,
		})
		return
	}
	response := SnapshotEntries{
		SnapshotId: result.SnapshotID, RepositoryId: result.RepositoryID, Path: result.Path,
		Items: make([]SnapshotEntry, 0, len(result.Entries)), Source: Cache,
		CollectedAt: result.CollectedAt, Truncated: result.Truncated,
	}
	for _, entry := range result.Entries {
		response.Items = append(response.Items, SnapshotEntry{
			Name: entry.Name, Path: entry.Path, Type: entry.Type, Size: entry.Size,
			Mode: entry.Mode, Uid: entry.UID, Gid: entry.GID, Mtime: entry.ModTime,
		})
	}
	a.json(w, http.StatusOK, response)
}

func (a *API) BackupPlanNow(w http.ResponseWriter, r *http.Request, planID openapi_types.UUID, params BackupPlanNowParams) {
	actor, ok := a.authorizeMutation(w, r, params.XCSRFToken, "PLAN_BACKUP_TRIGGER", "PLAN")
	if !ok {
		return
	}
	if r.URL.RawQuery != "" {
		a.problem(w, r, http.StatusBadRequest, "INVALID_REQUEST", "Invalid request", "The request is invalid.", nil)
		return
	}
	operation, err := a.control.QueueBackupNow(r.Context(), planID, actor.User, requestMeta(r))
	if err != nil {
		a.fleetProblem(w, r, err)
		return
	}
	a.json(w, http.StatusAccepted, backupOperationResponse(operation))
}
