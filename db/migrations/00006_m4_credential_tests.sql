-- +goose Up
create table operations (
  id uuid primary key,
  type text not null check (type = 'CREDENTIAL_TEST'),
  status text not null check (status in ('QUEUED','DISPATCHED','ACKNOWLEDGED','RUNNING','SUCCEEDED','SUCCEEDED_WITH_WARNINGS','FAILED','CANCEL_REQUESTED','CANCELED','TIMED_OUT','LOST','REJECTED')),
  source text not null check (source = 'USER'),
  storage_credential_id uuid not null references storage_credentials(id),
  secret_revision bigint not null check (secret_revision > 0),
  requested_by_user_id uuid not null references users(id),
  attempt integer not null default 1 check (attempt > 0),
  created_at timestamptz not null,
  dispatched_at timestamptz,
  acknowledged_at timestamptz,
  started_at timestamptz,
  finished_at timestamptz,
  error_code text not null default '',
  check ((status in ('SUCCEEDED','SUCCEEDED_WITH_WARNINGS','FAILED','CANCELED','TIMED_OUT','LOST','REJECTED')) = (finished_at is not null)),
  check (error_code in ('','CONNECTION_FAILED','TEST_TIMED_OUT','CONFIG_UNSAFE','REFRESH_FAILED','CREDENTIAL_CHANGED','CREDENTIAL_DISABLED','SECRET_UNAVAILABLE','WORKER_LOST'))
);
create index operations_created_idx on operations(created_at desc, id desc);
create unique index operations_active_credential_idx on operations(storage_credential_id)
  where finished_at is null;

create table operation_events (
  operation_id uuid not null references operations(id),
  sequence bigint not null check (sequence > 0),
  from_status text not null,
  to_status text not null,
  occurred_at timestamptz not null,
  primary key(operation_id, sequence)
);

create table jobs (
  id uuid primary key,
  operation_id uuid not null unique references operations(id),
  queue text not null check (queue = 'CREDENTIAL_TEST'),
  payload jsonb not null default '{}' check (payload = '{}'::jsonb),
  status text not null check (status in ('READY','LEASED','DONE','DEAD')),
  available_at timestamptz not null,
  lease_owner uuid,
  lease_expires_at timestamptz,
  attempt integer not null default 0 check (attempt >= 0),
  max_attempts integer not null default 3 check (max_attempts > 0),
  last_error_code text not null default '',
  created_at timestamptz not null,
  updated_at timestamptz not null,
  check ((lease_owner is null) = (lease_expires_at is null)),
  check ((status = 'LEASED') = (lease_owner is not null))
);
create index jobs_ready_idx on jobs(available_at, id) where status in ('READY','LEASED');

create table idempotency_records (
  scope_hash bytea not null check (octet_length(scope_hash)=32),
  key_hash bytea not null check (octet_length(key_hash)=32),
  request_hash bytea not null check (octet_length(request_hash)=32),
  status integer not null check (status=202),
  resource_type text not null check (resource_type='OPERATION'),
  resource_id uuid not null references operations(id),
  created_at timestamptz not null,
  expires_at timestamptz not null check (expires_at >= created_at + interval '24 hours'),
  primary key(scope_hash,key_hash)
);
alter table storage_credentials
  add column last_test_operation_id uuid references operations(id),
  add column last_tested_at timestamptz,
  add column last_test_result text not null default '',
  add column last_refreshed_at timestamptz;

revoke all on operations, operation_events, jobs, idempotency_records from public;
-- +goose StatementBegin
do $restfleet_permissions$
begin
  if exists (select 1 from pg_roles where rolname = 'restfleet_app') then
    execute 'grant select, insert, update on operations, jobs to restfleet_app';
    execute 'grant select, insert on operation_events to restfleet_app';
    execute 'grant select, insert, update on idempotency_records to restfleet_app';
  end if;
end
$restfleet_permissions$;
-- +goose StatementEnd

-- +goose Down
alter table storage_credentials
  drop column last_test_operation_id,
  drop column last_tested_at,
  drop column last_test_result,
  drop column last_refreshed_at;
drop table idempotency_records;
drop table jobs;
drop table operation_events;
drop table operations;
