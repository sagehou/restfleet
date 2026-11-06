package postgres

import (
 "context"
 "errors"
 "time"

 "github.com/google/uuid"
 "github.com/jackc/pgx/v5"
 "github.com/sagehou/restfleet/internal/domain"
)

// Caller holds repository and credential locks. A read may borrow the existing
// owner, never start a second cloud writer behind an unreleased admission.
func gatewayReadBinding(ctx context.Context,tx pgx.Tx,repo domain.Repository,operation uuid.UUID,configurationHash string) (*domain.GatewayReadBinding,error) {
 var id,repository uuid.UUID
 var runtime *uuid.UUID
 var hash string
 var valid bool
 err:=tx.QueryRow(ctx,`select a.id,a.repository_id,o.runtime_id,a.configuration_hash,
  coalesce(a.expires_at>clock_timestamp() and o.closed_at is null and o.runtime_id is not null
   and a.configuration_hash=$3 and r.archived_at is null and r.initialized_at is not null
   and r.status in ('PROVISIONING','READY','DEGRADED') and r.id=$2
   and a.host_id=r.host_id and a.gateway_id=r.gateway_username and a.restic_secret_ref=r.restic_secret_ref
   and a.gateway_secret_ref=r.gateway_secret_ref and a.storage_credential_id=r.storage_credential_id
   and ag.status='ACTIVE' and ag.host_id=a.host_id and h.status='ACTIVE' and h.archived_at is null
   and d.id=a.delivery_id and d.repository_id=r.id and d.accepted_at is not null
   and d.configuration_hash=$3 and d.gateway_secret_ref=r.gateway_secret_ref and d.restic_secret_ref=r.restic_secret_ref
   and not g.revoked and g.runtime_id=o.runtime_id and g.issued_at<=clock_timestamp() and g.expires_at>clock_timestamp(),false)
  from gateway_backup_admissions a
  left join gateway_pending_origins o on o.admission_id=a.id
  join repositories r on r.id=a.repository_id
  join agents ag on ag.id=a.agent_id
  join hosts h on h.id=a.host_id
  left join repository_agent_deliveries d on d.agent_id=a.agent_id
  left join lateral(select runtime_id,issued_at,expires_at,revoked from gateway_authorization_decisions where admission_id=a.id order by revision desc limit 1) g on true
  where a.storage_credential_id=$1 and a.released_at is null`,repo.StorageCredentialID,repo.ID,configurationHash).Scan(&id,&repository,&runtime,&hash,&valid)
 if errors.Is(err,pgx.ErrNoRows){return nil,nil};if err!=nil{return nil,err}
 if !valid || repository!=repo.ID || runtime==nil{return nil,domain.ErrRepositoryBusy}
 binding:=&domain.GatewayReadBinding{RepositoryID:repository,OperationID:operation,AdmissionID:id,RuntimeID:*runtime,ConfigurationHash:hash}
 if !binding.Valid(){return nil,domain.ErrRepositoryUnavailable}
 return binding,nil
}

func ensureReadJobBinding(ctx context.Context,tx pgx.Tx,job domain.CredentialJob) error {
 if job.GatewayRead==nil{return ensureNoBackupAdmission(ctx,tx,job.Credential.ID)}
 if job.Repository==nil || (job.Operation.Type!="SNAPSHOT_INDEX" && job.Operation.Type!="SNAPSHOT_LIST"){return domain.ErrOperationTransition}
 if job.Credential.Status=="DISABLED" || !job.RepositoryAvailable{return domain.ErrRepositoryUnavailable}
 current,err:=gatewayReadBinding(ctx,tx,*job.Repository,job.Operation.ID,job.GatewayRead.ConfigurationHash)
 if err!=nil{return err}
 if current==nil || *current!=*job.GatewayRead{return domain.ErrRepositoryUnavailable}
 return nil
}

// Retiring a read never frees the Gateway admission or changes provider health.
// Claim recovery may retire its own stale job without reacquiring data access.
func finishSnapshotReadJob(ctx context.Context,tx pgx.Tx,job *domain.CredentialJob,to string,now time.Time) error {
 if job.Repository==nil || (job.Operation.Type!="SNAPSHOT_INDEX" && job.Operation.Type!="SNAPSHOT_LIST"){return domain.ErrOperationTransition}
 if err:=transitionOperation(ctx,tx,&job.Operation,to,now);err!=nil{return err}
 audit,err:=credentialJobAudit(job.Operation,job.Operation.Type+"_RESULT",to,now)
 if err!=nil{return err}
 audit.ResourceType,audit.ResourceID="REPOSITORY",job.Repository.ID
 if to!="SUCCEEDED" && to!="SUCCEEDED_WITH_WARNINGS"{audit.Result=domain.AuditFailure}
 if err=appendAudit(ctx,tx,audit);err!=nil{return err}
 if job.Owner!=uuid.Nil {
  if err=ensureCredentialLease(ctx,tx,job.ID,job.Owner);err!=nil{return err}
  if err=ensureProvisionLease(ctx,tx,*job);err!=nil{return err}
 }
 if err=finishJobRow(ctx,tx,job,to,now);err!=nil{return err}
 _,err=tx.Exec(ctx,`delete from repository_leases where repository_id=$1 and operation_id=$2 and kind='MAINTENANCE'`,job.Repository.ID,job.Operation.ID)
 return err
}
