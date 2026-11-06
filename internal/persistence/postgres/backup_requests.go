package postgres

import (
 "bytes"
 "context"
 "encoding/binary"
 "encoding/json"
 "errors"

 "github.com/jackc/pgx/v5"
 "github.com/sagehou/restfleet/internal/domain"
)

func lockBackupRequest(ctx context.Context,tx pgx.Tx,key domain.BackupRequestKey) (domain.BackupOperation,bool,error) {
 var operation domain.BackupOperation
 if key.Scope==[32]byte{} || key.Key==[32]byte{} || key.Body==[32]byte{} {return operation,false,domain.ErrIdempotencyReused}
 if _,err:=tx.Exec(ctx,"select pg_advisory_xact_lock($1)",int64(binary.BigEndian.Uint64(key.Key[:8])));err!=nil{return operation,false,err}
 var body,raw []byte
 err:=tx.QueryRow(ctx,`select request_hash,response_json from backup_operation_requests
  where scope_hash=$1 and key_hash=$2 and expires_at>clock_timestamp()`,key.Scope[:],key.Key[:]).Scan(&body,&raw)
 if errors.Is(err,pgx.ErrNoRows){return operation,false,nil};if err!=nil{return operation,false,err}
 if !bytes.Equal(body,key.Body[:]){return operation,false,domain.ErrIdempotencyReused}
 if err:=json.Unmarshal(raw,&operation);err!=nil{return operation,false,err}
 return operation,true,nil
}

func saveBackupRequest(ctx context.Context,tx pgx.Tx,key domain.BackupRequestKey,operation domain.BackupOperation) error {
 raw,err:=json.Marshal(operation);if err!=nil{return err}
 _,err=tx.Exec(ctx,`insert into backup_operation_requests(scope_hash,key_hash,request_hash,operation_id,response_json,created_at,expires_at)
  values($1,$2,$3,$4,$5::jsonb,clock_timestamp(),clock_timestamp()+interval '24 hours')
  on conflict(scope_hash,key_hash) do update set request_hash=excluded.request_hash,operation_id=excluded.operation_id,
  response_json=excluded.response_json,created_at=excluded.created_at,expires_at=excluded.expires_at`,key.Scope[:],key.Key[:],key.Body[:],operation.ID,string(raw))
 return err
}
