-- +goose Up
alter table agents
  add column uptime_seconds bigint not null default 0,
  add column state_free_bytes bigint not null default 0,
  add column clock_offset_ms bigint not null default 0,
  add column heartbeat_error_code text not null default '',
  add column config_error_code text not null default '',
  add column config_error_field text not null default '',
  add constraint agents_revision_order_check check (accepted_revision <= desired_revision),
  add constraint agents_uptime_check check (uptime_seconds >= 0),
  add constraint agents_state_free_check check (state_free_bytes >= 0);

create table agent_desired_states (
  agent_id uuid not null references agents(id),
  revision bigint not null,
  generated_at timestamptz not null,
  config_hash text not null,
  config_json jsonb not null,
  created_at timestamptz not null,
  primary key (agent_id, revision),
  check (revision > 0),
  check (config_hash ~ '^sha256:[0-9a-f]{64}$'),
  check (jsonb_typeof(config_json) = 'object')
);

create table agent_inventories (
  id uuid primary key,
  agent_id uuid not null references agents(id),
  captured_at timestamptz not null,
  kernel text not null,
  os_release text not null,
  cpu_arch text not null,
  agent_version text not null,
  restic_version text not null,
  containerized boolean not null,
  available_bytes jsonb not null,
  clock_offset_ms bigint not null,
  capabilities jsonb not null,
  created_at timestamptz not null,
  check (char_length(kernel) <= 256),
  check (char_length(os_release) <= 256),
  check (cpu_arch in ('amd64', 'arm64')),
  check (char_length(agent_version) <= 64),
  check (char_length(restic_version) <= 64),
  check (jsonb_typeof(available_bytes) = 'object'),
  check (jsonb_typeof(capabilities) = 'array')
);

create index agent_inventories_agent_captured_idx
  on agent_inventories (agent_id, captured_at desc, id desc);

create table outbox_events (
  id uuid primary key,
  event_type text not null,
  aggregate_type text not null,
  aggregate_id uuid not null,
  payload jsonb not null,
  created_at timestamptz not null,
  available_at timestamptz not null,
  published_at timestamptz,
  attempt integer not null default 0,
  lease_owner text,
  lease_expires_at timestamptz,
  check (jsonb_typeof(payload) = 'object'),
  check (attempt >= 0),
  check ((lease_owner is null) = (lease_expires_at is null))
);

create index outbox_events_ready_idx
  on outbox_events (available_at, id)
  where published_at is null;

insert into agent_desired_states (
  agent_id, revision, generated_at, config_hash, config_json, created_at
)
select
  id,
  1,
  updated_at,
  'sha256:16db572cf66c08d0db851c5cba74041a651367e1a0086c50d05223576539e0fb',
  '{"plans":[],"repositories":[],"runtime_policy":{"max_parallel_io_jobs":1,"log_limit_bytes":10485760}}'::jsonb,
  updated_at
from agents;

update agents set desired_revision = 1 where desired_revision = 0;

-- +goose StatementBegin
do $restfleet_permissions$
begin
  if exists (select 1 from pg_roles where rolname = 'restfleet_app') then
    execute 'grant select, insert on agent_desired_states, agent_inventories to restfleet_app';
    execute 'grant select, insert, update on outbox_events to restfleet_app';
    execute 'grant usage on sequence audit_events_sequence_seq to restfleet_app';
  end if;
end
$restfleet_permissions$;
-- +goose StatementEnd

-- +goose Down
drop table outbox_events;
drop table agent_inventories;
drop table agent_desired_states;

alter table agents
  drop constraint agents_state_free_check,
  drop constraint agents_uptime_check,
  drop constraint agents_revision_order_check,
  drop column config_error_field,
  drop column config_error_code,
  drop column heartbeat_error_code,
  drop column clock_offset_ms,
  drop column state_free_bytes,
  drop column uptime_seconds;
