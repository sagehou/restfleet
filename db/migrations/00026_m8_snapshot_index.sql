-- +goose Up
-- Snapshots are keyed by repository and native snapshot id so out-of-band
-- snapshots can be indexed as UNMANAGED without inventing a Plan.
alter table snapshots
  drop constraint snapshots_pkey,
  alter column plan_id drop not null,
  alter column agent_id drop not null,
  alter column operation_id drop not null,
  drop constraint if exists snapshots_operation_id_fkey,
  add column managed boolean not null default true,
  add column tree_id text not null default '',
  add column tags text[] not null default '{}',
  add column paths text[] not null default '{}',
  add column program_version text not null default '',
  add column missing_at timestamptz,
  add constraint snapshots_pkey primary key (repository_id, snapshot_id),
  add constraint snapshots_tree_check check (tree_id = '' or tree_id ~ '^[0-9a-f]{64}$');
create index snapshots_repository_time_idx on snapshots(repository_id, captured_at desc, snapshot_id);
create index snapshots_host_time_idx on snapshots(host_id, captured_at desc);
create index snapshots_plan_time_idx on snapshots(plan_id, captured_at desc) where plan_id is not null;
create index snapshots_missing_idx on snapshots(repository_id) where missing_at is not null;

alter table operations drop constraint if exists operations_source_check,
  alter column requested_by_user_id drop not null,
  add constraint operations_source_check check (source in ('USER','SYSTEM')),
  add constraint operations_system_user_check check ((source='SYSTEM') = (requested_by_user_id is null)),
  drop constraint operations_type_check,
  drop constraint operations_error_code_check,
  drop constraint operations_repository_check,
  add constraint operations_type_check check (type in ('CREDENTIAL_TEST','REPOSITORY_INITIALIZE','SNAPSHOT_LIST','SNAPSHOT_INDEX')),
  add constraint operations_repository_check check ((type in ('REPOSITORY_INITIALIZE','SNAPSHOT_LIST','SNAPSHOT_INDEX')) = (repository_id is not null)),
  add constraint operations_error_code_check check (error_code in ('','CONNECTION_FAILED','TEST_TIMED_OUT','CONFIG_UNSAFE','REFRESH_FAILED','CREDENTIAL_CHANGED','CREDENTIAL_DISABLED','SECRET_UNAVAILABLE','WORKER_LOST','INITIALIZE_FAILED','INITIALIZE_TIMED_OUT','REPOSITORY_LOCKED','REPOSITORY_MISMATCH','REPOSITORY_NOT_EMPTY','PASSWORD_REJECTED','REPOSITORY_UNAVAILABLE','SNAPSHOT_NOT_FOUND','PATH_INVALID','SCAN_FAILED','SCAN_TIMED_OUT','INDEX_FAILED','INDEX_TIMED_OUT'));
alter table jobs drop constraint jobs_queue_check,
  drop constraint jobs_payload_check,
  add constraint jobs_queue_check check (queue in ('CREDENTIAL_TEST','REPOSITORY_INITIALIZE','SNAPSHOT_LIST','SNAPSHOT_INDEX')),
  add constraint jobs_payload_check check (payload = '{}'::jsonb or queue = 'SNAPSHOT_LIST');

-- +goose Down
-- The old snapshot primary key cannot represent UNMANAGED rows; fail closed.
drop index snapshots_missing_idx;
drop index snapshots_plan_time_idx;
drop index snapshots_host_time_idx;
drop index snapshots_repository_time_idx;
alter table snapshots
  drop constraint snapshots_tree_check,
  drop constraint snapshots_pkey,
  drop column missing_at,
  drop column program_version,
  drop column paths,
  drop column tags,
  drop column tree_id,
  drop column managed,
  alter column operation_id set not null,
  alter column agent_id set not null,
  alter column plan_id set not null,
  add constraint snapshots_pkey primary key (plan_id, snapshot_id),
  add constraint snapshots_operation_id_fkey foreign key (operation_id) references backup_operations(id);
alter table jobs drop constraint jobs_queue_check,
  drop constraint jobs_payload_check,
  add constraint jobs_queue_check check (queue in ('CREDENTIAL_TEST','REPOSITORY_INITIALIZE','SNAPSHOT_LIST')),
  add constraint jobs_payload_check check (payload = '{}'::jsonb or queue = 'SNAPSHOT_LIST');
alter table operations drop constraint operations_system_user_check,
  drop constraint operations_source_check,
  drop constraint operations_type_check,
  drop constraint operations_error_code_check,
  drop constraint operations_repository_check,
  add constraint operations_source_check check (source='USER'),
  add constraint operations_type_check check (type in ('CREDENTIAL_TEST','REPOSITORY_INITIALIZE','SNAPSHOT_LIST')),
  add constraint operations_repository_check check ((type in ('REPOSITORY_INITIALIZE','SNAPSHOT_LIST')) = (repository_id is not null)),
  add constraint operations_error_code_check check (error_code in ('','CONNECTION_FAILED','TEST_TIMED_OUT','CONFIG_UNSAFE','REFRESH_FAILED','CREDENTIAL_CHANGED','CREDENTIAL_DISABLED','SECRET_UNAVAILABLE','WORKER_LOST','INITIALIZE_FAILED','INITIALIZE_TIMED_OUT','REPOSITORY_LOCKED','REPOSITORY_MISMATCH','REPOSITORY_NOT_EMPTY','PASSWORD_REJECTED','REPOSITORY_UNAVAILABLE','SNAPSHOT_NOT_FOUND','PATH_INVALID','SCAN_FAILED','SCAN_TIMED_OUT')),
  alter column requested_by_user_id set not null;
