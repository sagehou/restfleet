-- +goose Up
-- M4 §7.10 Step 1: Bounded offline Gateway authorization grants.
-- Schema 11 gateway_backup_admissions expires_at is NOT updatable; this table
-- supports renewal via monotonic authorization_sequence and explicit renewal_at.
-- Authorization trust comes from the protected central material channel (HMAC
-- key materialized to Gateway tmpfs), NOT from a self-selected public key in
-- the grant payload.

create table offline_authorizations (
    id                    uuid primary key,
    owner                 uuid not null,
    gateway_instance_id   uuid not null,
    agent_id              uuid not null references agents(id),
    host_id               uuid not null references hosts(id),
    repository_id         uuid not null references repositories(id),
    gateway_id            uuid not null,
    storage_credential_id uuid not null references storage_credentials(id),
    delivery_id           uuid not null,
    configuration_hash    text not null check (configuration_hash ~ '^[0-9a-f]{64}$'),
    authorized_at         timestamptz not null,
    expires_at            timestamptz not null,
    authorization_sequence bigint not null default 1 check (authorization_sequence > 0),
    renewed_at            timestamptz,
    revoked_at            timestamptz,
    disabled_at           timestamptz,
    created_at            timestamptz not null,
    check (expires_at > authorized_at),
    check (expires_at <= authorized_at + interval '12 hours'),
    check (renewed_at is null or renewed_at >= authorized_at),
    check (revoked_at is null or revoked_at >= authorized_at),
    check (disabled_at is null or disabled_at >= authorized_at)
);

-- Per-Gateway instance: at most one active offline authorization.
create unique index offline_auth_gateway_instance_idx
    on offline_authorizations(gateway_instance_id)
    where revoked_at is null and disabled_at is null;

-- Per-Host: at most one active offline authorization.
create unique index offline_auth_host_idx
    on offline_authorizations(host_id)
    where revoked_at is null and disabled_at is null;

-- Per-Repository: at most one active offline authorization.
create unique index offline_auth_repository_idx
    on offline_authorizations(repository_id)
    where revoked_at is null and disabled_at is null;

-- Per-StorageCredential: at most one active offline authorization.
create unique index offline_auth_credential_idx
    on offline_authorizations(storage_credential_id)
    where revoked_at is null and disabled_at is null;

-- Authorization sequence anti-rollback: gateway_instance + sequence is unique
-- so the Gateway can detect replay of older grants.
create unique index offline_auth_sequence_idx
    on offline_authorizations(gateway_instance_id, authorization_sequence);

revoke all on offline_authorizations from public;
-- +goose StatementBegin
do $permissions$ begin
    if exists(select 1 from pg_roles where rolname='restfleet_app') then
        grant select, insert, update on offline_authorizations to restfleet_app;
    end if;
end $permissions$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
do $$ begin
    if exists(select 1 from offline_authorizations) then
        raise exception 'offline authorization history exists; use a forward migration';
    end if;
end $$;
-- +goose StatementEnd
drop table offline_authorizations;
