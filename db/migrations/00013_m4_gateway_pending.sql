-- +goose Up
create table gateway_pending_origins (
    runtime_id uuid not null,
    admission_id uuid not null unique references gateway_backup_admissions(id),
    public_key bytea not null check (octet_length(public_key)=32),
    initial_secret_revision bigint not null check (initial_secret_revision>0),
    created_at timestamptz not null default clock_timestamp(),
    closed_at timestamptz check (closed_at is null or closed_at>=created_at),
    primary key (admission_id,runtime_id),
    check (substr(runtime_id::text,15,1)='7' and substr(runtime_id::text,20,1) in ('8','9','a','b'))
);
create table gateway_pending_records (
    admission_id uuid not null,
    runtime_id uuid not null,
    sequence bigint not null check (sequence>0),
    record_id uuid not null unique,
    wire_hash text not null check (wire_hash ~ '^[0-9a-f]{64}$'),
    authorization_revision bigint not null check (authorization_revision>0),
    kind text not null check (kind in ('audit','refresh')),
    occurred_at timestamptz not null,
    committed_at timestamptz not null default clock_timestamp(),
    primary key (admission_id,runtime_id,sequence),
    foreign key (admission_id,runtime_id) references gateway_pending_origins(admission_id,runtime_id),
    check (substr(record_id::text,15,1)='7' and substr(record_id::text,20,1) in ('8','9','a','b'))
);
revoke all on gateway_pending_origins, gateway_pending_records from public;
-- +goose StatementBegin
do $permissions$ begin
    if exists(select 1 from pg_roles where rolname='restfleet_app') then
        grant select,insert on gateway_pending_origins, gateway_pending_records to restfleet_app;
        grant update(closed_at) on gateway_pending_origins to restfleet_app;
    end if;
end $permissions$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
do $$ begin
    if exists(select 1 from gateway_pending_origins) then
        raise exception 'gateway pending history exists; use a forward migration';
    end if;
end $$;
-- +goose StatementEnd
drop table gateway_pending_records;
drop table gateway_pending_origins;
