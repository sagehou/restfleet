package httpapi

import (
	"net/http"

	openapi_types "github.com/oapi-codegen/runtime/types"
)

func (a *API) ListBackupOperationLogs(w http.ResponseWriter,r *http.Request,id openapi_types.UUID,params ListBackupOperationLogsParams) {
	if _,ok:=a.authorizeRead(w,r);!ok{return}
	for key,values:=range r.URL.Query(){if (key!="after_sequence" && key!="limit") || len(values)!=1{a.problem(w,r,http.StatusBadRequest,"INVALID_REQUEST","Invalid request","The log cursor is invalid.",nil);return}}
	after,limit:=int64(0),100
	if params.AfterSequence!=nil{after=*params.AfterSequence}
	if params.Limit!=nil{limit=*params.Limit}
	if after<0 || after>1<<62 || limit<1 || limit>200{a.problem(w,r,http.StatusBadRequest,"INVALID_REQUEST","Invalid request","The log cursor is invalid.",nil);return}
	entries,err:=a.control.OperationLogs(r.Context(),id,uint64(after),limit)
	if err!=nil{a.fleetProblem(w,r,err);return}
	page:=BackupLogPage{Items:make([]BackupLogEntry,0,len(entries)),NextSequence:after}
	for _,e:=range entries {
		page.Items=append(page.Items,BackupLogEntry{Sequence:int64(e.Sequence),ObservedAt:e.ObservedAt,Section:BackupLogEntrySection(e.Section),Level:BackupLogEntryLevel(e.Level),Code:e.Code,FilesDone:e.FilesDone,BytesDone:e.BytesDone})
		page.NextSequence=int64(e.Sequence)
	}
	a.json(w,http.StatusOK,page)
}
