-- +goose Up
create table repository_agent_deliveries (
    agent_id uuid primary key references agents(id),
    id uuid not null unique,
    repository_id uuid not null references repositories(id),
    revision bigint not null check (revision > 0),
    configuration_hash text not null check (configuration_hash ~ '^[0-9a-f]{64}$'),
    gateway_secret_ref uuid not null references secrets(id),
    restic_secret_ref uuid not null references secrets(id),
    created_at timestamptz not null,
    accepted_at timestamptz,
    check (gateway_secret_ref <> restic_secret_ref)
);

revoke all on repository_agent_deliveries from public;
-- +goose StatementBegin
do $permissions$ begin
    if exists(select 1 from pg_roles where rolname='restfleet_app') then
        grant select, insert, update on repository_agent_deliveries to restfleet_app;
    end if;
end $permissions$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
do $$ begin
    if exists(select 1 from repository_agent_deliveries) then
        raise exception 'credential delivery history exists; use a forward migration';
    end if;
end $$;
-- +goose StatementEnd
drop table repository_agent_deliveries;
