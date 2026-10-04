-- +goose Up
create table gateway_audit_origins (
    id uuid primary key,
    runtime_id uuid not null unique,
    public_key bytea not null check (octet_length(public_key)=32),
    created_at timestamptz not null default clock_timestamp(),
    closed_at timestamptz check (closed_at is null or closed_at>=created_at),
    unique(id,runtime_id),
    check (substr(id::text,15,1)='7' and substr(id::text,20,1) in ('8','9','a','b')),
    check (substr(runtime_id::text,15,1)='7' and substr(runtime_id::text,20,1) in ('8','9','a','b'))
);
create table gateway_audit_records (
    origin_id uuid not null,
    runtime_id uuid not null,
    sequence bigint not null check (sequence>0),
    record_id uuid not null unique,
    wire_hash text not null check (wire_hash ~ '^[0-9a-f]{64}$'),
    occurred_at timestamptz not null,
    committed_at timestamptz not null default clock_timestamp(),
    primary key(origin_id,runtime_id,sequence),
    foreign key(origin_id,runtime_id) references gateway_audit_origins(id,runtime_id),
    check (substr(record_id::text,15,1)='7' and substr(record_id::text,20,1) in ('8','9','a','b'))
);
revoke all on gateway_audit_origins, gateway_audit_records from public;
-- +goose StatementBegin
do $permissions$ begin
    if exists(select 1 from pg_roles where rolname='restfleet_app') then
        grant select,insert on gateway_audit_origins, gateway_audit_records to restfleet_app;
        grant update(closed_at) on gateway_audit_origins to restfleet_app;
    end if;
end $permissions$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
do $$ begin
    if exists(select 1 from gateway_audit_origins) then
        raise exception 'gateway global audit history exists; use a forward migration';
    end if;
end $$;
-- +goose StatementEnd
drop table gateway_audit_records;
drop table gateway_audit_origins;
