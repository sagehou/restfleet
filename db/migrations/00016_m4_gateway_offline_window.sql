-- +goose Up
-- Widen durations without changing existing deadlines or releasing any fence.
alter table gateway_backup_admissions
    drop constraint gateway_backup_admissions_check1,
    add constraint gateway_backup_admission_lifetime_check
        check (expires_at >= created_at + interval '1 minute' and expires_at <= created_at + interval '30 days');
alter table gateway_authorization_decisions
    drop constraint gateway_authorization_decisions_lifetime_seconds_check,
    add constraint gateway_authorization_decisions_lifetime_seconds_check
        check (lifetime_seconds between 0 and 2592000);

-- +goose Down
-- Longer-lived history makes this rollback fail atomically; use a forward fix.
alter table gateway_backup_admissions
    drop constraint gateway_backup_admission_lifetime_check,
    add constraint gateway_backup_admissions_check1
        check (expires_at >= created_at + interval '1 minute' and expires_at <= created_at + interval '24 hours');
alter table gateway_authorization_decisions
    drop constraint gateway_authorization_decisions_lifetime_seconds_check,
    add constraint gateway_authorization_decisions_lifetime_seconds_check
        check (lifetime_seconds between 0 and 43200);
