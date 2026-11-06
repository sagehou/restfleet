package httpapi

import (
 "bytes"
 "context"
 "crypto/sha256"
 "fmt"
 "net/http"
 "net/http/httptest"
 "strings"
 "sync"
 "testing"
 "time"

 "github.com/google/uuid"
 "github.com/jackc/pgx/v5/pgxpool"
 "github.com/sagehou/restfleet/internal/domain"
 "github.com/sagehou/restfleet/internal/persistence/postgres"
)

func testBackupRequest(value string) domain.BackupRequestKey {
 return domain.BackupRequestKey{Scope:sha256.Sum256([]byte("test-backup-scope")),Key:sha256.Sum256([]byte(value)),Body:sha256.Sum256(nil)}
}

func backupFeedbackFixture(t *testing.T) (*postgres.Store,*pgxpool.Pool,*testBrowser,AgentEnrollmentResponse,uuid.UUID) {
 t.Helper()
 store,pool,b,enrolled,repository,_:=deliveryFixture(t,"onedrive")
 ctx:=context.Background()
 now:=time.Now().UTC()
 var userID uuid.UUID
 if err:=pool.QueryRow(ctx,"select id from users limit 1").Scan(&userID);err!=nil{t.Fatal(err)}
 templateID,planID:=uuid.Must(uuid.NewV7()),uuid.Must(uuid.NewV7())
 hash:=strings.Repeat("e",64)
 if _,err:=pool.Exec(ctx,`insert into templates(id,name,current_revision,created_at,updated_at) values($1,$2,1,$3,$3)`,templateID,templateID.String(),now);err!=nil{t.Fatal(err)}
 if _,err:=pool.Exec(ctx,`insert into template_revisions(template_id,revision,config_json,config_hash,created_by,created_at) values($1,1,'{}'::jsonb,$2,$3,$4)`,templateID,hash,userID,now);err!=nil{t.Fatal(err)}
 if _,err:=pool.Exec(ctx,`insert into plans(id,name,host_id,agent_id,repository_id,template_id,template_revision,effective_config,config_hash,status,desired_revision,accepted_revision,created_at,updated_at)
  values($1,'backup-feedback',$2,$3,$4,$5,1,'{}'::jsonb,$6,'ACTIVE',1,0,$7,$7)`,planID,repository.HostId,enrolled.AgentId,repository.Id,templateID,hash,now);err!=nil{t.Fatal(err)}
 return store,pool,b,enrolled,planID
}

func TestBackupHTTPRequestsAreAtomicAndIdempotent(t *testing.T) {
 _,pool,b,_,planID:=backupFeedbackFixture(t)
 ctx:=context.Background()
 path:="/api/v1/plans/"+planID.String()+"/backup"
 headers:=func(key string)map[string]string{return map[string]string{"X-CSRF-Token":b.cookies[csrfCookieName].Value,"Idempotency-Key":key}}
 var wg sync.WaitGroup
 ids:=make(chan uuid.UUID,12)
 for i:=0;i<12;i++ {wg.Add(1);go func(){defer wg.Done()
  request:=httptest.NewRequest(http.MethodPost,path,nil)
  request.RemoteAddr=b.remote
  for key,value:=range headers("same-backup"){request.Header.Set(key,value)}
  for _,cookie:=range b.cookies{request.AddCookie(cookie)}
  response:=httptest.NewRecorder();b.handler.ServeHTTP(response,request)
  if response.Code!=http.StatusAccepted{t.Errorf("backup = %d: %s",response.Code,response.Body.String());return}
  var operation BackupOperation;decodeResponse(t,response,&operation);ids<-operation.Id
 }()}
 wg.Wait();close(ids)
 var original uuid.UUID
 for id:=range ids{if original==uuid.Nil{original=id};if id!=original{t.Fatal("duplicate side effect")}}
 if original==uuid.Nil{t.Fatal("no accepted backup")}
 var count int
 if err:=pool.QueryRow(ctx,"select count(*) from backup_operations where plan_id=$1",planID).Scan(&count);err!=nil || count!=1{t.Fatalf("backup count: %d %v",count,err)}
 if err:=pool.QueryRow(ctx,"select count(*) from audit_events where action='PLAN_BACKUP_TRIGGER' and resource_id=$1",planID).Scan(&count);err!=nil || count!=1{t.Fatalf("audit duplicated: %d %v",count,err)}
 cancelPath:="/api/v1/backup-operations/"+original.String()+"/cancel"
 secret:="secret-canary-cancel-reason"
 first:=b.request(t,http.MethodPost,cancelPath,BackupOperationCancel{Reason:&secret},headers("same-cancel"))
 if first.Code!=http.StatusOK || bytes.Contains(first.Body.Bytes(),[]byte(secret)){t.Fatalf("cancel did not project safe metadata: %d",first.Code)}
 second:=b.request(t,http.MethodPost,cancelPath,BackupOperationCancel{Reason:&secret},headers("same-cancel"))
 if second.Code!=http.StatusOK || !bytes.Equal(first.Body.Bytes(),second.Body.Bytes()){t.Fatal("terminal cancellation did not replay original response")}
 different:="a different reason"
 if response:=b.request(t,http.MethodPost,cancelPath,BackupOperationCancel{Reason:&different},headers("same-cancel"));response.Code!=http.StatusConflict{t.Fatalf("different body reused key: %d",response.Code)}
 if _,err:=pool.Exec(ctx,"update plans set status='PAUSED' where id=$1",planID);err!=nil{t.Fatal(err)}
 replay:=b.request(t,http.MethodPost,path,nil,headers("same-backup"))
 var queued BackupOperation;decodeResponse(t,replay,&queued)
 if replay.Code!=http.StatusAccepted || queued.Id!=original || queued.Status!=BackupOperationStatusQUEUED{t.Fatal("plan/state change broke request replay")}
 if _,err:=pool.Exec(ctx,"update plans set status='ACTIVE' where id=$1",planID);err!=nil{t.Fatal(err)}
 retryPath:="/api/v1/backup-operations/"+original.String()+"/retry"
 retry:=b.request(t,http.MethodPost,retryPath,nil,headers("same-retry"))
 if retry.Code!=http.StatusAccepted{t.Fatalf("retry = %d %s",retry.Code,retry.Body.String())}
 retryAgain:=b.request(t,http.MethodPost,retryPath,nil,headers("same-retry"))
 if retryAgain.Code!=http.StatusAccepted || !bytes.Equal(retry.Body.Bytes(),retryAgain.Body.Bytes()){t.Fatal("retry created another operation")}
 var retried BackupOperation;decodeResponse(t,retry,&retried)
 if retried.ParentOperationId==nil || *retried.ParentOperationId!=original || retried.Attempt==nil || *retried.Attempt!=2{t.Fatal("retry linkage lost")}
 for _,request:=range []struct{path string;body any;key string}{{path,map[string]string{"unexpected":"body"},"body"},{retryPath,map[string]string{},"body"},{cancelPath,nil,""}} {
  requestHeaders:=headers(request.key);if request.key==""{delete(requestHeaders,"Idempotency-Key")}
  response:=b.request(t,http.MethodPost,request.path,request.body,requestHeaders);if response.Code!=http.StatusBadRequest{t.Errorf("invalid request accepted: %d",response.Code)}
 }
}

func TestOperationLogBindingSequenceReplayAndTransactionalACK(t *testing.T) {
 store,pool,b,enrolled,planID:=backupFeedbackFixture(t)
 ctx:=context.Background();id:=uuid.Must(uuid.NewV7());now:=time.Now().UTC()
 if err:=store.UpsertOperationProgress(ctx,domain.BackupOperationProgress{OperationID:id,PlanID:planID,AgentID:enrolled.AgentId,PlanRevision:1,ScheduledAt:now,ObservedAt:now,Stage:"backup",Sequence:1});err!=nil{t.Fatal(err)}
 entry:=domain.BackupLogEntry{Sequence:1,ObservedAt:now,Section:"backup",Level:"INFO",Code:"BACKUP_STARTED"}
 if _,err:=store.AppendOperationLogs(ctx,uuid.Must(uuid.NewV7()),id,[]domain.BackupLogEntry{entry});err==nil{t.Fatal("cross-Agent log accepted")}
 bad:=entry;bad.Code="secret-canary-token"
 if _,err:=store.AppendOperationLogs(ctx,enrolled.AgentId,id,[]domain.BackupLogEntry{bad});err==nil{t.Fatal("raw error string accepted")}
 if ack,err:=store.AppendOperationLogs(ctx,enrolled.AgentId,id,[]domain.BackupLogEntry{entry});err!=nil || ack!=1{t.Fatalf("first ACK %d %v",ack,err)}
 second:=entry;second.Sequence=2;second.Code="BACKUP_PROGRESS"
 duplicate:=entry;duplicate.BytesDone=999
 if ack,err:=store.AppendOperationLogs(ctx,enrolled.AgentId,id,[]domain.BackupLogEntry{duplicate,second});err!=nil || ack!=2{t.Fatalf("overlap %d %v",ack,err)}
 gap:=entry;gap.Sequence=4
 if _,err:=store.AppendOperationLogs(ctx,enrolled.AgentId,id,[]domain.BackupLogEntry{gap});err==nil{t.Fatal("gap accepted")}
 if _,err:=pool.Exec(ctx,`create function reject_backup_log() returns trigger language plpgsql as $$ begin raise exception 'log insert unavailable'; end $$;
  create trigger reject_backup_log before insert on backup_operation_logs for each row execute function reject_backup_log();`);err!=nil{t.Fatal(err)}
 third:=entry;third.Sequence=3;third.Code="BACKUP_SUMMARY"
 if ack,err:=store.AppendOperationLogs(ctx,enrolled.AgentId,id,[]domain.BackupLogEntry{third});err==nil || ack!=0{t.Fatal("ACK escaped failed transaction")}
 if _,err:=pool.Exec(ctx,"drop trigger reject_backup_log on backup_operation_logs; drop function reject_backup_log()");err!=nil{t.Fatal(err)}
 if ack,err:=store.AppendOperationLogs(ctx,enrolled.AgentId,id,[]domain.BackupLogEntry{third});err!=nil || ack!=3{t.Fatal("failed insert advanced durable head")}
 response:=b.request(t,http.MethodGet,fmt.Sprintf("/api/v1/backup-operations/%s/logs?limit=1",id),nil,nil)
 if response.Code!=http.StatusOK{t.Fatalf("logs API: %d %s",response.Code,response.Body.String())}
 var page BackupLogPage;decodeResponse(t,response,&page)
 if len(page.Items)!=1 || page.NextSequence!=1 || page.Items[0].BytesDone!=0{t.Fatal("duplicate rewrote first committed log")}
 response=b.request(t,http.MethodGet,fmt.Sprintf("/api/v1/backup-operations/%s/logs?after_sequence=1&limit=2",id),nil,nil)
 decodeResponse(t,response,&page)
 if len(page.Items)!=2 || page.Items[0].Sequence!=2 || page.NextSequence!=3{t.Fatal("cursor skipped logs")}
 for _,query:=range []string{"limit=201","limit=1&limit=2","unknown=x","after_sequence=-1"} {
  if response:=b.request(t,http.MethodGet,fmt.Sprintf("/api/v1/backup-operations/%s/logs?%s",id,query),nil,nil);response.Code!=http.StatusBadRequest{t.Errorf("bad log query accepted: %s %d",query,response.Code)}
 }
}
