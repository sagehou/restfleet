package httpapi

import (
 "bytes"
 "context"
 "crypto/tls"
 "errors"
 "strings"
 "testing"
 "time"

 "github.com/google/uuid"
 "github.com/sagehou/restfleet/internal/domain"
 "github.com/sagehou/restfleet/internal/restic"
 "github.com/sagehou/restfleet/internal/security"
 control "github.com/sagehou/restfleet/internal/server"
)

func TestGatewayReadJobReusesOwnerWithoutCloudSecretOrRefresh(t *testing.T) {
 f:=pendingIntegrationFixture(t,"onedrive")
 ctx:=context.Background();binding:=f.statement.Binding
 operation,err:=f.store.EnqueueSnapshotIndex(ctx,domain.Operation{ID:uuid.Must(uuid.NewV7()),RepositoryID:&binding.RepositoryID},binding.ConfigurationHash)
 if err!=nil{t.Fatal(err)}
 owner:=uuid.Must(uuid.NewV7())
 job,err:=f.store.ClaimCredentialJob(ctx,owner);if err!=nil{t.Fatal(err)}
 if job.GatewayRead==nil || job.GatewayRead.RepositoryID!=binding.RepositoryID || job.GatewayRead.AdmissionID!=binding.AdmissionID || job.GatewayRead.RuntimeID!=binding.RuntimeID || job.GatewayRead.OperationID!=operation.ID {t.Fatal("read changed owner binding")}
 var cloud,repo,signed int
 if err=f.pool.QueryRow(ctx,`select count(*) filter(where action='STORAGE_SECRET_ACCESS'),count(*) filter(where action='REPOSITORY_SECRET_ACCESS'),count(*) filter(where action='GATEWAY_CENTRAL_READ_AUTHORIZATION') from audit_events where request_id=$1`,operation.ID).Scan(&cloud,&repo,&signed);err!=nil{t.Fatal(err)}
 if cloud!=0 || repo!=1 || signed!=1{t.Fatal("read access audit crossed secret boundary")}
 if _,err=f.store.RefreshCredentialJob(ctx,job.ID,owner,job.Credential.SecretRevision,domain.SecretEnvelope{});!errors.Is(err,domain.ErrOperationTransition){t.Fatal("reader could update cloud secret")}
 // A refresh by the sole owner must not invalidate this read's metadata lease.
 if _,err=f.pool.Exec(ctx,`update storage_credentials set secret_revision=secret_revision+1 where id=$1`,binding.StorageCredentialID);err!=nil{t.Fatal(err)}
 if err=f.store.RenewCredentialJob(ctx,job.ID,owner);err!=nil{t.Fatal(err)}
 if err=f.store.FinishSnapshotIndexJob(ctx,job.ID,owner,"",domain.SnapshotIndexResult{});err!=nil{t.Fatal(err)}
 var released bool;var leases int
 if err=f.pool.QueryRow(ctx,"select released_at is not null from gateway_backup_admissions where id=$1",binding.AdmissionID).Scan(&released);err!=nil{t.Fatal(err)}
 if err=f.pool.QueryRow(ctx,"select count(*) from repository_leases where operation_id=$1",operation.ID).Scan(&leases);err!=nil{t.Fatal(err)}
 if released || leases!=0{t.Fatal("read cleanup released owner or leaked read lease")}
 if err=f.store.VerifyAuditChain(ctx);err!=nil{t.Fatal(err)}
}

func TestGatewayReadInvalidationRetiresJobWithoutIndexOrQueueStarvation(t *testing.T) {
 for _,when:=range []string{"queued","running"} {
  t.Run(when,func(t *testing.T){
   f:=pendingIntegrationFixture(t,"onedrive");ctx:=context.Background();binding:=f.statement.Binding
   op,err:=f.store.EnqueueSnapshotIndex(ctx,domain.Operation{ID:uuid.Must(uuid.NewV7()),RepositoryID:&binding.RepositoryID},binding.ConfigurationHash);if err!=nil{t.Fatal(err)}
   owner:=uuid.Must(uuid.NewV7());var job domain.CredentialJob
   if when=="running"{job,err=f.store.ClaimCredentialJob(ctx,owner);if err!=nil{t.Fatal(err)}}
   if _,err=f.pool.Exec(ctx,"update gateway_authorization_decisions set revoked=true,expires_at=null,lifetime_seconds=0 where admission_id=$1",binding.AdmissionID);err!=nil{t.Fatal(err)}
   if when=="queued" {
    if _,err=f.store.ClaimCredentialJob(ctx,owner);!errors.Is(err,domain.ErrNotFound){t.Fatal("invalid read was claimed")}
   }else{
    if err=f.store.RenewCredentialJob(ctx,job.ID,owner);err==nil{t.Fatal("revoked read renewed")}
    result:=domain.SnapshotIndexResult{Entries:[]domain.IndexedSnapshot{{SnapshotID:strings.Repeat("a",64),TreeID:strings.Repeat("b",64),CapturedAt:time.Now().UTC()}}}
    if err=f.store.FinishSnapshotIndexJob(ctx,job.ID,owner,"",result);err!=nil{t.Fatal(err)}
   }
   stored,err:=f.store.Operation(ctx,op.ID);if err!=nil{t.Fatal(err)}
   if !domain.OperationTerminal(stored.Status) || stored.ErrorCode!="REPOSITORY_UNAVAILABLE"{t.Fatal("invalid read left active operation")}
   var count int
   if err=f.pool.QueryRow(ctx,"select count(*) from snapshots where operation_id=$1",op.ID).Scan(&count);err!=nil || count!=0{t.Fatal("revoked reader committed an index")}
   credential:=createCredential(t,f.browser,"Unrelated queued test")
   queued:=queueCredentialTest(t,f.browser,credential.Id,"after-invalid-read")
   next,err:=f.store.ClaimCredentialJob(ctx,uuid.Must(uuid.NewV7()));if err!=nil || next.Operation.ID!=queued.Id{t.Fatal("retired reader blocked next job")}
   var released bool
   if err=f.pool.QueryRow(ctx,"select released_at is not null from gateway_backup_admissions where id=$1",binding.AdmissionID).Scan(&released);err!=nil || released{t.Fatal("failed read freed owner fence")}
  })
 }
}

func TestGatewayReadEnqueueRejectsExpiredOrChangedOwner(t *testing.T) {
 for _,change:=range []string{"hash","delivery","runtime","expiry"} {
  t.Run(change,func(t *testing.T){
   f:=pendingIntegrationFixture(t,"onedrive");ctx:=context.Background();b:=f.statement.Binding
   switch change {
   case "hash":b.ConfigurationHash=strings.Repeat("f",64)
   case "delivery":if _,err:=f.pool.Exec(ctx,"update repository_agent_deliveries set accepted_at=null where agent_id=$1",b.AgentID);err!=nil{t.Fatal(err)}
   case "runtime":if _,err:=f.pool.Exec(ctx,"update gateway_pending_origins set runtime_id=$2 where admission_id=$1",b.AdmissionID,uuid.Must(uuid.NewV7()));err!=nil{t.Fatal(err)}
   case "expiry":if _,err:=f.pool.Exec(ctx,"update gateway_backup_admissions set created_at=clock_timestamp()-interval '2 hours',expires_at=clock_timestamp()-interval '1 hour' where id=$1",b.AdmissionID);err!=nil{t.Fatal(err)}
   }
   if _,err:=f.store.EnqueueSnapshotIndex(ctx,domain.Operation{ID:uuid.Must(uuid.NewV7()),RepositoryID:&b.RepositoryID},b.ConfigurationHash);!errors.Is(err,domain.ErrRepositoryBusy){t.Fatal("invalid owner bypassed fence")}
  })
 }
}

func TestGatewayReadWorkerNeverDecryptsCloudMaterial(t *testing.T) {
 f:=pendingIntegrationFixture(t,"onedrive");ctx:=context.Background();binding:=f.statement.Binding
 var bundle []byte
 if err:=f.control.WithAgentCredential(ctx,binding.AgentID,true,func(d domain.RepositoryCredential)error{bundle=bytes.Clone(d.CABundlePEM);return nil});err!=nil{t.Fatal(err)}
 ca,key,err:=security.NewAgentCA(time.Now());if err!=nil{t.Fatal(err)};defer clear(key)
 called:=false
 worker,err:=control.NewControlPlane(f.store,control.Settings{MasterKey:bytes.Repeat([]byte{8},32),GatewayPublicURL:"https://gateway.example",
  Enrollment:control.EnrollmentSettings{CA:ca,ServerCABundlePEM:bundle},
  IndexSnapshots:func(_ context.Context,r restic.SnapshotIndexRequest,persist func(context.Context,[]byte)error)(domain.SnapshotIndexResult,error){
   called=true
   if r.Gateway==nil || len(r.Config)!=0 || r.Remote!="" || persist!=nil || len(r.Password)!=43{t.Fatal("worker exposed cloud configuration or lost Restic password")}
   certificate,err:=tls.X509KeyPair(r.Gateway.CertificatePEM,r.Gateway.PrivateKeyPEM);if err!=nil{t.Fatal(err)}
   reader,err:=security.GatewayReaderFromCertificate(certificate.Leaf)
   if err!=nil || reader.AdmissionID!=binding.AdmissionID || reader.RuntimeID!=binding.RuntimeID || reader.RepositoryID!=binding.RepositoryID{t.Fatal("worker signed wrong owner")}
   return domain.SnapshotIndexResult{},nil
  }})
 if err!=nil{t.Fatal(err)}
 operation,err:=f.store.EnqueueSnapshotIndex(ctx,domain.Operation{ID:uuid.Must(uuid.NewV7()),RepositoryID:&binding.RepositoryID},binding.ConfigurationHash);if err!=nil{t.Fatal(err)}
 // Deliberately unusable cloud ciphertext: central reads must never open it.
 if _,err=f.pool.Exec(ctx,`update secrets set ciphertext='\x01'::bytea where id=(select secret_ref from storage_credentials where id=$1)`,binding.StorageCredentialID);err!=nil{t.Fatal(err)}
 worked,err:=worker.ProcessCredentialJob(ctx,uuid.Must(uuid.NewV7()));if err!=nil || !worked || !called{t.Fatal("central reader tried direct-cloud path")}
 result,err:=f.store.Operation(ctx,operation.ID);if err!=nil || result.Status!="SUCCEEDED"{t.Fatal("read worker did not complete")}
}
