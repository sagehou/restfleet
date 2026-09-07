-- +goose Up
alter table repositories
  add column restic_id text check (restic_id ~ '^[0-9a-f]{64}$'),
  add column initialized_at timestamptz,
  add column last_initialize_operation_id uuid references operations(id),
  add constraint repositories_initialized_check check ((initialized_at is not null) = (restic_id is not null) and (initialized_at is null or format_version is not distinct from 2));
alter table operations
  drop constraint operations_type_check,
  drop constraint operations_error_code_check,
  add column repository_id uuid references repositories(id),
  add constraint operations_type_check check (type in ('CREDENTIAL_TEST','REPOSITORY_INITIALIZE')),
  add constraint operations_repository_check check ((type='REPOSITORY_INITIALIZE') = (repository_id is not null)),
  add constraint operations_error_code_check check (error_code in ('','CONNECTION_FAILED','TEST_TIMED_OUT','CONFIG_UNSAFE','REFRESH_FAILED','CREDENTIAL_CHANGED','CREDENTIAL_DISABLED','SECRET_UNAVAILABLE','WORKER_LOST','INITIALIZE_FAILED','INITIALIZE_TIMED_OUT','REPOSITORY_LOCKED','REPOSITORY_MISMATCH','REPOSITORY_NOT_EMPTY','PASSWORD_REJECTED','REPOSITORY_UNAVAILABLE'));
alter table jobs drop constraint jobs_queue_check,
  add constraint jobs_queue_check check (queue in ('CREDENTIAL_TEST','REPOSITORY_INITIALIZE'));
create unique index operations_active_repository_idx on operations(repository_id) where finished_at is null;
create table repository_leases (
  repository_id uuid primary key references repositories(id),
  operation_id uuid not null references operations(id),
  owner uuid not null,
  kind text not null check (kind in ('MAINTENANCE','BACKUP')),
  expires_at timestamptz not null
);
revoke all on repository_leases from public;
-- +goose StatementBegin
do $permissions$
begin
  if exists (select 1 from pg_roles where rolname='restfleet_app') then
    grant select, insert, update on repository_leases to restfleet_app;
    grant update(restic_id,format_version,initialized_at,last_initialize_operation_id,revision,updated_at) on repositories to restfleet_app;
  end if;
end
$permissions$;
-- +goose StatementEnd

-- +goose Down
-- Fail closed before dropping any initialization history, including failed jobs.
alter table operations add constraint operations_legacy_only check (type='CREDENTIAL_TEST');
drop table repository_leases;
drop index operations_active_repository_idx;
alter table jobs drop constraint jobs_queue_check,
  add constraint jobs_queue_check check (queue='CREDENTIAL_TEST');
alter table operations drop constraint operations_legacy_only, drop constraint operations_repository_check,
  drop constraint operations_type_check, drop constraint operations_error_code_check, drop column repository_id,
  add constraint operations_type_check check (type='CREDENTIAL_TEST'),
  add constraint operations_error_code_check check (error_code in ('','CONNECTION_FAILED','TEST_TIMED_OUT','CONFIG_UNSAFE','REFRESH_FAILED','CREDENTIAL_CHANGED','CREDENTIAL_DISABLED','SECRET_UNAVAILABLE','WORKER_LOST'));
alter table repositories drop constraint repositories_initialized_check,
  drop column restic_id, drop column initialized_at, drop column last_initialize_operation_id;
