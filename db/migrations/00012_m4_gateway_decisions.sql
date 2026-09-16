-- +goose Up
create table gateway_authorization_decisions (
    admission_id uuid not null references gateway_backup_admissions(id),
    revision bigint not null check (revision > 0),
    request_id uuid not null unique,
    runtime_id uuid not null,
    lifetime_seconds bigint not null check (lifetime_seconds between 0 and 43200),
    issued_at timestamptz not null,
    expires_at timestamptz,
    revoked boolean not null,
    primary key (admission_id, revision),
    check (substr(request_id::text,15,1)='7' and substr(request_id::text,20,1) in ('8','9','a','b')),
    check (substr(runtime_id::text,15,1)='7' and substr(runtime_id::text,20,1) in ('8','9','a','b')),
    check (issued_at > '1970-01-01 UTC'::timestamptz and issued_at = date_trunc('second',issued_at)),
    check ((revoked and expires_at is null and lifetime_seconds=0) or
           (not revoked and expires_at is not null and lifetime_seconds>0 and
            expires_at=date_trunc('second',expires_at) and expires_at>issued_at and
            expires_at<=issued_at+make_interval(secs=>lifetime_seconds::double precision)))
);
revoke all on gateway_authorization_decisions from public;
-- +goose StatementBegin
do $permissions$ begin
    if exists(select 1 from pg_roles where rolname='restfleet_app') then
        grant select, insert on gateway_authorization_decisions to restfleet_app;
    end if;
end $permissions$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
do $$ begin
    if exists(select 1 from gateway_authorization_decisions) then
        raise exception 'gateway decision history exists; use a forward migration';
    end if;
end $$;
-- +goose StatementEnd
drop table gateway_authorization_decisions;
