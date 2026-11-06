-- +goose Up
alter table backup_operations add column attempt integer not null default 1 check (attempt between 1 and 10000);
alter table backup_operations add column summary_json jsonb;
alter table plans add column last_result_at timestamptz;
update plans p set last_result_at=o.finished_at from backup_operations o where o.id=p.last_operation_id;
create table backup_operation_log_heads (
  operation_id uuid primary key references backup_operations(id),
  ack_sequence bigint not null default 0 check (ack_sequence>=0),
  bytes_received bigint not null default 0 check (bytes_received between 0 and 10485760)
);
create table backup_operation_logs (
  operation_id uuid not null references backup_operations(id),
  sequence bigint not null check (sequence>0),
  entry_json jsonb not null,
  primary key(operation_id,sequence)
);
create table backup_operation_requests (
  scope_hash bytea not null check (octet_length(scope_hash)=32),
  key_hash bytea not null check (octet_length(key_hash)=32),
  request_hash bytea not null check (octet_length(request_hash)=32),
  operation_id uuid not null references backup_operations(id),
  response_json jsonb not null,
  created_at timestamptz not null,
  expires_at timestamptz not null check (expires_at>=created_at+interval '24 hours'),
  primary key(scope_hash,key_hash)
);
revoke all on backup_operation_log_heads,backup_operation_logs,backup_operation_requests from public;
-- +goose StatementBegin
do $restfleet_permissions$
begin
  if exists(select 1 from pg_roles where rolname='restfleet_app') then
    execute 'grant select,insert,update on backup_operation_log_heads to restfleet_app';
    execute 'grant select,insert on backup_operation_logs to restfleet_app';
    execute 'grant select,insert,update on backup_operation_requests to restfleet_app';
  end if;
end
$restfleet_permissions$;
-- +goose StatementEnd

-- +goose Down
drop table backup_operation_requests;
drop table backup_operation_logs;
drop table backup_operation_log_heads;
alter table backup_operations drop column attempt;
alter table backup_operations drop column summary_json;
alter table plans drop column last_result_at;
