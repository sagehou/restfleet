-- +goose Up
-- M4 §7.10 Step 2-3: Durable write-back records for independent Gateway.
-- Records are encrypted by the Gateway before submission; the server stores
-- them opaque. Each record carries trusted owner/instance binding, a unique
-- identifier and an ordered version for replay detection.

create table writeback_records (
    id                  uuid primary key,
    authorization_id    uuid not null references offline_authorizations(id),
    owner               uuid not null,
    gateway_instance_id uuid not null,
    sequence            bigint not null check (sequence > 0),
    record_type         text not null check (record_type in ('AUDIT_EVENT','TOKEN_REFRESH','ADMISSION_CHANGE')),
    payload             bytea not null,
    checksum            bytea not null check (octet_length(checksum) = 32),
    created_at          timestamptz not null,
    confirmed_at        timestamptz,
    check (confirmed_at is null or confirmed_at >= created_at)
);

create unique index writeback_instance_sequence_idx
    on writeback_records(gateway_instance_id, sequence);

create index writeback_pending_idx
    on writeback_records(gateway_instance_id, sequence)
    where confirmed_at is null;

revoke all on writeback_records from public;
-- +goose StatementBegin
do $permissions$ begin
    if exists(select 1 from pg_roles where rolname='restfleet_app') then
        grant select, insert, update on writeback_records to restfleet_app;
    end if;
end $permissions$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
do $$ begin
    if exists(select 1 from writeback_records) then
        raise exception 'writeback record history exists; use a forward migration';
    end if;
end $$;
-- +goose StatementEnd
drop table writeback_records;
