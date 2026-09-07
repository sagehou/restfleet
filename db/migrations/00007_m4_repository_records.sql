-- +goose Up
create table repositories (
  id uuid primary key,
  host_id uuid not null references hosts(id),
  storage_credential_id uuid not null references storage_credentials(id),
  name text not null check (char_length(name) between 1 and 128),
  backend_path text not null unique,
  gateway_username uuid not null unique,
  gateway_secret_ref uuid not null unique references secrets(id),
  gateway_secret_revision bigint not null check (gateway_secret_revision > 0),
  restic_secret_ref uuid not null unique references secrets(id),
  restic_secret_revision bigint not null check (restic_secret_revision > 0),
  format_version integer check (format_version = 2),
  status text not null check (status in ('PROVISIONING','READY','DEGRADED','LOCKED','DISABLED','ERROR')),
  revision bigint not null default 1 check (revision > 0),
  created_at timestamptz not null,
  updated_at timestamptz not null,
  archived_at timestamptz,
  check (backend_path = 'restfleet/agents/' || gateway_username::text || '/' || id::text),
  check (gateway_secret_ref <> restic_secret_ref)
);
-- Disabled and failed repositories still own their Host until explicitly archived.
create unique index repositories_host_idx on repositories(host_id) where archived_at is null;
create index repositories_storage_idx on repositories(storage_credential_id);

create table repository_credential_revisions (
  id uuid primary key,
  repository_id uuid not null references repositories(id),
  kind text not null check (kind in ('GATEWAY','RESTIC_KEY')),
  revision bigint not null check (revision > 0),
  secret_ref uuid not null unique references secrets(id),
  valid_from timestamptz not null,
  retire_after timestamptz,
  retired_at timestamptz,
  created_at timestamptz not null,
  unique(repository_id,kind,revision)
);

revoke all on repositories, repository_credential_revisions from public;
-- +goose StatementBegin
do $restfleet_permissions$
begin
  if exists (select 1 from pg_roles where rolname = 'restfleet_app') then
    execute 'grant select, insert on repositories, repository_credential_revisions to restfleet_app';
  end if;
end
$restfleet_permissions$;
-- +goose StatementEnd

-- +goose Down
drop table repository_credential_revisions;
drop table repositories;
