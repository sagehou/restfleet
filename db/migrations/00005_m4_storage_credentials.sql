-- +goose Up
create table storage_credentials (
  id uuid primary key,
  name text not null,
  provider text not null check (provider = 'RCLONE_ONEDRIVE'),
  remote_name text not null,
  status text not null check (status in ('UNTESTED','HEALTHY','DEGRADED','EXPIRED','DISABLED')),
  secret_ref uuid not null references secrets(id),
  secret_revision bigint not null check (secret_revision > 0),
  revision bigint not null default 1 check (revision > 0),
  created_at timestamptz not null,
  updated_at timestamptz not null,
  check (char_length(name) between 1 and 128),
  check (remote_name ~ '^[A-Za-z][A-Za-z0-9_-]{1,63}$')
);
create unique index storage_credentials_name_key on storage_credentials (lower(name));
create table storage_credential_revisions (
  credential_id uuid not null references storage_credentials(id),
  revision bigint not null check (revision > 0),
  secret_ref uuid not null unique references secrets(id),
  created_at timestamptz not null,
  primary key (credential_id, revision)
);
revoke all on storage_credentials, storage_credential_revisions from public;
-- +goose StatementBegin
do $restfleet_permissions$
begin
  if exists (select 1 from pg_roles where rolname = 'restfleet_app') then
    execute 'grant select, insert, update on storage_credentials to restfleet_app';
    execute 'grant select, insert on storage_credential_revisions to restfleet_app';
  end if;
end
$restfleet_permissions$;
-- +goose StatementEnd

-- +goose Down
drop table storage_credential_revisions;
drop table storage_credentials;
