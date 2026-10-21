-- +goose Up
alter table operations drop constraint if exists operations_legacy_only;
alter table operations
  drop constraint operations_type_check,
  drop constraint operations_error_code_check,
  drop constraint operations_repository_check,
  add constraint operations_type_check check (type in ('CREDENTIAL_TEST','REPOSITORY_INITIALIZE','SNAPSHOT_LIST')),
  add constraint operations_repository_check check ((type in ('REPOSITORY_INITIALIZE','SNAPSHOT_LIST')) = (repository_id is not null)),
  add constraint operations_error_code_check check (error_code in ('','CONNECTION_FAILED','TEST_TIMED_OUT','CONFIG_UNSAFE','REFRESH_FAILED','CREDENTIAL_CHANGED','CREDENTIAL_DISABLED','SECRET_UNAVAILABLE','WORKER_LOST','INITIALIZE_FAILED','INITIALIZE_TIMED_OUT','REPOSITORY_LOCKED','REPOSITORY_MISMATCH','REPOSITORY_NOT_EMPTY','PASSWORD_REJECTED','REPOSITORY_UNAVAILABLE','SNAPSHOT_NOT_FOUND','PATH_INVALID','SCAN_FAILED','SCAN_TIMED_OUT'));
alter table jobs drop constraint jobs_queue_check,
  drop constraint jobs_payload_check,
  add constraint jobs_queue_check check (queue in ('CREDENTIAL_TEST','REPOSITORY_INITIALIZE','SNAPSHOT_LIST')),
  add constraint jobs_payload_check check (payload = '{}'::jsonb or queue = 'SNAPSHOT_LIST');

create table snapshot_list_results (
  operation_id uuid primary key references operations(id),
  repository_id uuid not null references repositories(id),
  snapshot_id text not null check (snapshot_id ~ '^[0-9a-f]{64}$'),
  tree_id text not null check (tree_id ~ '^[0-9a-f]{64}$'),
  path text not null check (char_length(path) between 1 and 4096),
  truncated boolean not null,
  scanned integer not null check (scanned >= 0),
  entries jsonb not null check (jsonb_typeof(entries) = 'array' and jsonb_array_length(entries) <= 5000),
  collected_at timestamptz not null,
  expires_at timestamptz not null,
  check (expires_at > collected_at)
);
create index snapshot_list_cache_idx on snapshot_list_results(repository_id, snapshot_id, path, expires_at desc);

revoke all on snapshot_list_results from public;
-- +goose StatementBegin
do $permissions$
begin
  if exists (select 1 from pg_roles where rolname='restfleet_app') then
    grant select, insert, update, delete on snapshot_list_results to restfleet_app;
  end if;
end
$permissions$;
-- +goose StatementEnd

-- +goose Down
-- Fail closed before dropping snapshot listing history.
alter table operations add constraint operations_legacy_only check (type in ('CREDENTIAL_TEST','REPOSITORY_INITIALIZE'));
drop table snapshot_list_results;
alter table jobs drop constraint jobs_queue_check,
  drop constraint jobs_payload_check,
  add constraint jobs_queue_check check (queue in ('CREDENTIAL_TEST','REPOSITORY_INITIALIZE')),
  add constraint jobs_payload_check check (payload = '{}'::jsonb);
alter table operations drop constraint operations_type_check,
  drop constraint operations_error_code_check,
  drop constraint operations_repository_check,
  add constraint operations_type_check check (type in ('CREDENTIAL_TEST','REPOSITORY_INITIALIZE')),
  add constraint operations_repository_check check ((type='REPOSITORY_INITIALIZE') = (repository_id is not null)),
  add constraint operations_error_code_check check (error_code in ('','CONNECTION_FAILED','TEST_TIMED_OUT','CONFIG_UNSAFE','REFRESH_FAILED','CREDENTIAL_CHANGED','CREDENTIAL_DISABLED','SECRET_UNAVAILABLE','WORKER_LOST','INITIALIZE_FAILED','INITIALIZE_TIMED_OUT','REPOSITORY_LOCKED','REPOSITORY_MISMATCH','REPOSITORY_NOT_EMPTY','PASSWORD_REJECTED','REPOSITORY_UNAVAILABLE'));
