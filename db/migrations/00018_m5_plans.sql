-- +goose Up
create table plans (
  id uuid primary key,
  name text not null check (char_length(name) between 1 and 128),
  host_id uuid not null references hosts(id),
  agent_id uuid not null references agents(id),
  repository_id uuid not null references repositories(id),
  template_id uuid not null references templates(id),
  template_revision bigint not null check (template_revision > 0),
  overrides jsonb not null default '{}'::jsonb,
  effective_config jsonb not null,
  config_hash text not null check (config_hash ~ '^[0-9a-f]{64}$'),
  status text not null check (status in ('DRAFT','PENDING_APPLY','ACTIVE','PAUSED','INVALID','ARCHIVED')),
  desired_revision bigint not null default 1 check (desired_revision > 0),
  accepted_revision bigint not null default 0 check (accepted_revision >= 0 and accepted_revision <= desired_revision),
  last_scheduled_at timestamptz,
  next_run_at timestamptz,
  last_success_at timestamptz,
  last_operation_id uuid,
  backup_health text not null default 'UNKNOWN' check (backup_health in ('UNKNOWN','HEALTHY','OVERDUE','FAILED')),
  archived_at timestamptz,
  created_at timestamptz not null,
  updated_at timestamptz not null,
  check ((status = 'ARCHIVED') = (archived_at is not null))
);
create unique index plans_host_name_idx on plans(host_id, lower(name)) where archived_at is null;
create index plans_host_idx on plans(host_id);
create index plans_repository_idx on plans(repository_id);

create table plan_revisions (
  plan_id uuid not null references plans(id),
  revision bigint not null check (revision > 0),
  effective_config_json jsonb not null,
  config_hash text not null check (config_hash ~ '^[0-9a-f]{64}$'),
  created_by uuid not null references users(id),
  created_at timestamptz not null,
  primary key (plan_id, revision)
);

revoke all on plans, plan_revisions from public;
-- +goose StatementBegin
do $restfleet_permissions$
begin
  if exists (select 1 from pg_roles where rolname = 'restfleet_app') then
    execute 'grant select, insert, update on plans to restfleet_app';
    execute 'grant select, insert on plan_revisions to restfleet_app';
  end if;
end
$restfleet_permissions$;
-- +goose StatementEnd

-- +goose Down
drop table plan_revisions;
drop table plans;
