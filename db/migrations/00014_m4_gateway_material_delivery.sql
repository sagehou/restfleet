-- +goose Up
create table gateway_material_deliveries (
    admission_id uuid primary key,
    runtime_id uuid not null,
    challenge_hash text not null check (challenge_hash ~ '^[0-9a-f]{64}$'),
    authorization_revision bigint not null check (authorization_revision>0),
    secret_revision bigint not null check (secret_revision>0),
    created_at timestamptz not null default clock_timestamp(),
    foreign key (admission_id,runtime_id) references gateway_pending_origins(admission_id,runtime_id)
);
revoke all on gateway_material_deliveries from public;
-- +goose StatementBegin
do $permissions$ begin
    if exists(select 1 from pg_roles where rolname='restfleet_app') then
        grant select,insert on gateway_material_deliveries to restfleet_app;
    end if;
end $permissions$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
do $$ begin
    if exists(select 1 from gateway_material_deliveries) then
        raise exception 'gateway delivery history exists; use a forward migration';
    end if;
end $$;
-- +goose StatementEnd
drop table gateway_material_deliveries;
