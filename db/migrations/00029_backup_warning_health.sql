-- +goose Up
alter table plans drop constraint plans_backup_health_check,
  add constraint plans_backup_health_check
  check (backup_health in ('UNKNOWN','HEALTHY','WARNING','OVERDUE','FAILED','NEVER_RUN','PAUSED'));

update plans p set backup_health='WARNING'
where p.backup_health='HEALTHY' and 'SUCCEEDED_WITH_WARNINGS'=(
  select o.status from backup_operations o where o.plan_id=p.id
    and o.status in ('SUCCEEDED','SUCCEEDED_WITH_WARNINGS','FAILED','LOST','MISSED')
  order by o.finished_at desc,o.id desc limit 1
);

-- +goose Down
-- Refuse a downgrade while WARNING rows would lose their meaning.
alter table plans drop constraint plans_backup_health_check,
  add constraint plans_backup_health_check
  check (backup_health in ('UNKNOWN','HEALTHY','OVERDUE','FAILED','NEVER_RUN','PAUSED'));
