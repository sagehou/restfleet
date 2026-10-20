-- +goose Up
create table backup_operations (
  id uuid primary key,
  plan_id uuid not null references plans(id),
  agent_id uuid not null references agents(id),
  type text not null default 'BACKUP' check (type in ('BACKUP','RESTORE','SNAPSHOT_INDEX','DOWNLOAD','CHECK','FORGET','PRUNE','MAINTENANCE')),
  source text not null check (source in ('SCHEDULED','MANUAL')),
  status text not null check (status in ('QUEUED','RUNNING','SUCCEEDED','SUCCEEDED_WITH_WARNINGS','FAILED','CANCELED','MISSED','CONCURRENCY_SKIPPED')),
  plan_revision bigint not null check (plan_revision > 0),
  config_hash text not null default '',
  scheduled_at timestamptz,
  started_at timestamptz,
  finished_at timestamptz,
  exit_code integer,
  error_code text not null default '',
  snapshot_id text not null default '',
  files_done bigint not null default 0 check (files_done >= 0),
  total_files bigint not null default 0 check (total_files >= 0),
  bytes_done bigint not null default 0 check (bytes_done >= 0),
  total_bytes bigint not null default 0 check (total_bytes >= 0),
  created_at timestamptz not null,
  updated_at timestamptz not null
);
create index backup_operations_plan_idx on backup_operations(plan_id, scheduled_at desc);

create table backup_operation_events (
  operation_id uuid not null references backup_operations(id),
  sequence bigint not null check (sequence >= 0),
  stage text not null default '',
  files_done bigint not null default 0 check (files_done >= 0),
  total_files bigint not null default 0 check (total_files >= 0),
  bytes_done bigint not null default 0 check (bytes_done >= 0),
  total_bytes bigint not null default 0 check (total_bytes >= 0),
  observed_at timestamptz not null,
  primary key (operation_id, sequence)
);

revoke all on backup_operations, backup_operation_events from public;
-- +goose StatementBegin
do $restfleet_permissions$
begin
  if exists (select 1 from pg_roles where rolname = 'restfleet_app') then
    execute 'grant select, insert, update on backup_operations to restfleet_app';
    execute 'grant select, insert on backup_operation_events to restfleet_app';
  end if;
end
$restfleet_permissions$;
-- +goose StatementEnd

-- +goose Down
drop table backup_operation_events;
drop table backup_operations;
