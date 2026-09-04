-- +goose Up
create table hosts (
  id uuid primary key,
  display_name text not null,
  description text not null default '',
  labels jsonb not null default '{}'::jsonb,
  timezone text not null,
  status text not null,
  revision bigint not null default 1,
  created_at timestamptz not null,
  updated_at timestamptz not null,
  archived_at timestamptz,
  check (char_length(display_name) between 1 and 128),
  check (char_length(description) <= 1024),
  check (jsonb_typeof(labels) = 'object'),
  check (status in ('PENDING', 'ACTIVE', 'DISABLED', 'REVOKED')),
  check (revision > 0)
);

create unique index hosts_display_name_active_key
  on hosts (lower(display_name))
  where archived_at is null;

create table agents (
  id uuid primary key,
  host_id uuid not null references hosts(id),
  install_id uuid not null unique,
  public_key_fingerprint text not null unique,
  certificate_serial text not null unique,
  certificate_not_after timestamptz not null,
  status text not null,
  version text not null,
  protocol_version text not null,
  os text not null,
  arch text not null,
  hostname text not null,
  boot_id text not null default '',
  restic_version text not null default '',
  last_seen_at timestamptz,
  last_connected_at timestamptz,
  desired_revision bigint not null default 0,
  accepted_revision bigint not null default 0,
  last_error_code text,
  last_error_at timestamptz,
  created_at timestamptz not null,
  updated_at timestamptz not null,
  check (status in ('ACTIVE', 'REVOKED')),
  check (os = 'linux'),
  check (arch in ('amd64', 'arm64')),
  check (desired_revision >= 0),
  check (accepted_revision >= 0)
);

create unique index agents_one_active_per_host
  on agents (host_id)
  where status = 'ACTIVE';

create table enrollment_tokens (
  id uuid primary key,
  host_id uuid not null references hosts(id),
  token_hash bytea not null unique,
  token_fingerprint text not null,
  expires_at timestamptz not null,
  created_by uuid not null references users(id),
  created_at timestamptz not null,
  used_at timestamptz,
  used_by_agent_id uuid references agents(id),
  revoked_at timestamptz,
  check (octet_length(token_hash) = 32),
  check (char_length(token_fingerprint) = 5),
  check (expires_at > created_at),
  check ((used_at is null) = (used_by_agent_id is null)),
  check (not (used_at is not null and revoked_at is not null))
);

create index enrollment_tokens_host_created_idx
  on enrollment_tokens (host_id, created_at desc);

create table agent_certificates (
  id uuid primary key,
  agent_id uuid not null references agents(id),
  serial_number text not null unique,
  public_key_fingerprint text not null,
  not_before timestamptz not null,
  not_after timestamptz not null,
  issued_at timestamptz not null,
  revoked_at timestamptz,
  revocation_reason text not null default '',
  superseded_by uuid references agent_certificates(id),
  overlap_ends_at timestamptz,
  check (not_after > not_before),
  check ((revoked_at is null) or char_length(revocation_reason) > 0)
);

create index agent_certificates_agent_idx
  on agent_certificates (agent_id, issued_at desc);

create table secrets (
  id uuid primary key,
  kind text not null,
  algorithm text not null,
  key_id text not null,
  ciphertext bytea not null,
  nonce bytea not null,
  wrapped_data_key bytea not null,
  wrap_nonce bytea not null,
  aad bytea not null,
  created_at timestamptz not null,
  check (algorithm = 'AES-256-GCM+AES-256-GCM'),
  check (octet_length(nonce) = 12),
  check (octet_length(wrap_nonce) = 12)
);

create table server_pki (
  singleton boolean primary key default true,
  ca_certificate_pem bytea not null,
  private_key_secret_id uuid not null references secrets(id),
  created_at timestamptz not null,
  check (singleton)
);

revoke update, delete, truncate on agent_certificates, secrets, server_pki from public;

-- +goose StatementBegin
do $restfleet_permissions$
begin
  if exists (select 1 from pg_roles where rolname = 'restfleet_app') then
    execute 'grant select, insert, update on hosts, agents, enrollment_tokens to restfleet_app';
    execute 'grant select, insert, update on agent_certificates to restfleet_app';
    execute 'grant select, insert on secrets, server_pki to restfleet_app';
  end if;
end
$restfleet_permissions$;
-- +goose StatementEnd

-- +goose Down
drop table server_pki;
drop table secrets;
drop table agent_certificates;
drop table enrollment_tokens;
drop table agents;
drop table hosts;
