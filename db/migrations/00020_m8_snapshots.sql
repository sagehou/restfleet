-- +goose Up
create table snapshots (
  plan_id uuid not null references plans(id),
  snapshot_id text not null check (char_length(snapshot_id) between 1 and 128),
  agent_id uuid not null references agents(id),
  host_id uuid not null references hosts(id),
  repository_id uuid not null references repositories(id),
  operation_id uuid not null references backup_operations(id),
  captured_at timestamptz not null,
  recorded_at timestamptz not null,
  files_new bigint not null default 0 check (files_new >= 0),
  bytes_added bigint not null default 0 check (bytes_added >= 0),
  primary key (plan_id, snapshot_id)
);
create index snapshots_recorded_idx on snapshots(recorded_at desc);

revoke all on snapshots from public;
-- +goose StatementBegin
do $restfleet_permissions$
begin
  if exists (select 1 from pg_roles where rolname = 'restfleet_app') then
    execute 'grant select, insert, update on snapshots to restfleet_app';
  end if;
end
$restfleet_permissions$;
-- +goose StatementEnd

-- +goose Down
drop table snapshots;
