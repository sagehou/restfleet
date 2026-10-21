-- +goose Up
alter table plans drop constraint if exists plans_backup_health_check,
  add constraint plans_backup_health_check
  check (backup_health in ('UNKNOWN','HEALTHY','OVERDUE','FAILED','NEVER_RUN','PAUSED'));

-- +goose Down
-- The old constraint rejects NEVER_RUN/PAUSED rows, failing closed.
alter table plans drop constraint plans_backup_health_check,
  add constraint plans_backup_health_check
  check (backup_health in ('UNKNOWN','HEALTHY','OVERDUE','FAILED'));
