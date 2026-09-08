-- +goose Up
create table gateway_backup_admissions (
    id uuid primary key,
    owner uuid not null,
    agent_id uuid not null references agents(id),
    host_id uuid not null references hosts(id),
    repository_id uuid not null references repositories(id),
    gateway_id uuid not null,
    storage_credential_id uuid not null references storage_credentials(id),
    delivery_id uuid not null,
    gateway_secret_ref uuid not null references secrets(id),
    restic_secret_ref uuid not null references secrets(id),
    configuration_hash text not null check (configuration_hash ~ '^[0-9a-f]{64}$'),
    created_at timestamptz not null,
    expires_at timestamptz not null,
    released_at timestamptz,
    check (gateway_secret_ref <> restic_secret_ref),
    check (expires_at >= created_at + interval '1 minute' and expires_at <= created_at + interval '24 hours'),
    check (released_at is null or released_at >= created_at)
);
-- Expiry alone MUST NOT release the data-plane fence. Keep history and require
-- the trusted owner to confirm all request/process cleanup before release.
create unique index gateway_admission_repository_idx on gateway_backup_admissions(repository_id) where released_at is null;
create unique index gateway_admission_host_idx on gateway_backup_admissions(host_id) where released_at is null;
create unique index gateway_admission_agent_idx on gateway_backup_admissions(agent_id) where released_at is null;
create unique index gateway_admission_gateway_idx on gateway_backup_admissions(gateway_id) where released_at is null;
create unique index gateway_admission_credential_idx on gateway_backup_admissions(storage_credential_id) where released_at is null;

revoke all on gateway_backup_admissions from public;
-- +goose StatementBegin
do $permissions$ begin
    if exists(select 1 from pg_roles where rolname='restfleet_app') then
        grant select, insert on gateway_backup_admissions to restfleet_app;
        grant update(released_at) on gateway_backup_admissions to restfleet_app;
    end if;
end $permissions$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
do $$ begin
    if exists(select 1 from gateway_backup_admissions) then
        raise exception 'backup admission history exists; use a forward migration';
    end if;
end $$;
-- +goose StatementEnd
drop table gateway_backup_admissions;
