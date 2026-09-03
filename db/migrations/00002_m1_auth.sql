-- +goose Up
create table users (
  id uuid primary key,
  username text not null,
  display_name text not null,
  password_hash text not null,
  role text not null,
  status text not null,
  last_login_at timestamptz,
  created_at timestamptz not null,
  updated_at timestamptz not null,
  check (char_length(username) between 3 and 64),
  check (char_length(display_name) between 1 and 128),
  check (role in ('ADMIN', 'VIEWER')),
  check (status in ('ACTIVE', 'DISABLED'))
);

create unique index users_username_lower_key on users (lower(username));

create table bootstrap_state (
  singleton boolean primary key default true,
  completed_at timestamptz,
  completed_by uuid references users(id),
  created_at timestamptz not null,
  check (singleton),
  check ((completed_at is null) = (completed_by is null))
);

insert into bootstrap_state (singleton, created_at) values (true, now());

create table sessions (
  id uuid primary key,
  user_id uuid not null references users(id),
  token_hash bytea not null unique,
  csrf_secret_hash bytea not null,
  created_at timestamptz not null,
  last_seen_at timestamptz not null,
  idle_expires_at timestamptz not null,
  absolute_expires_at timestamptz not null,
  revoked_at timestamptz,
  ip_hash bytea not null,
  user_agent_summary text not null default '',
  check (octet_length(token_hash) = 32),
  check (octet_length(csrf_secret_hash) = 32),
  check (octet_length(ip_hash) = 32),
  check (char_length(user_agent_summary) <= 256),
  check (idle_expires_at > created_at),
  check (absolute_expires_at > created_at),
  check (idle_expires_at <= absolute_expires_at)
);

create index sessions_user_active_idx
  on sessions (user_id, absolute_expires_at)
  where revoked_at is null;

create table audit_events (
  id uuid primary key,
  sequence bigint generated always as identity unique,
  occurred_at timestamptz not null,
  actor_type text not null,
  actor_id uuid,
  action text not null,
  resource_type text not null,
  resource_id uuid,
  request_id uuid not null,
  source_ip_hash bytea,
  result text not null,
  reason_code text not null,
  changes jsonb not null default '{}'::jsonb,
  previous_hash bytea not null,
  event_hash bytea not null,
  check (actor_type in ('USER', 'AGENT', 'SYSTEM')),
  check (result in ('SUCCESS', 'DENIED', 'FAILURE')),
  check (octet_length(previous_hash) in (0, 32)),
  check (octet_length(event_hash) = 32),
  check (source_ip_hash is null or octet_length(source_ip_hash) = 32)
);

create index audit_events_occurred_at_idx on audit_events (occurred_at desc, sequence desc);
create index audit_events_actor_idx on audit_events (actor_type, actor_id, occurred_at desc);

-- The runtime role is expected to receive INSERT/SELECT only on audit_events.
revoke update, delete, truncate on audit_events from public;

-- Grant the runtime role only the M1 privileges when the deployment created it.
-- +goose StatementBegin
do $restfleet_permissions$
begin
  if exists (select 1 from pg_roles where rolname = 'restfleet_app') then
    execute 'grant select, insert, update on users, bootstrap_state, sessions to restfleet_app';
    execute 'grant select, insert on audit_events to restfleet_app';
    execute 'grant usage, select on sequence audit_events_sequence_seq to restfleet_app';
    execute 'grant select on goose_db_version to restfleet_app';
  end if;
end
$restfleet_permissions$;
-- +goose StatementEnd

-- +goose Down
drop table audit_events;
drop table sessions;
drop table bootstrap_state;
drop table users;
