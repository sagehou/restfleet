-- +goose Up
-- A new boot id proves the previous Agent process is gone. The timestamp lets
-- the Server give the Agent a grace window to replay a recovered result before
-- old-boot RUNNING operations are closed as LOST.
alter table agents add column boot_changed_at timestamptz;
alter table backup_operations drop constraint backup_operations_status_check,
  add constraint backup_operations_status_check check (status in ('QUEUED','RUNNING','SUCCEEDED','SUCCEEDED_WITH_WARNINGS','FAILED','CANCEL_REQUESTED','CANCELED','LOST','MISSED','CONCURRENCY_SKIPPED'));
create index backup_operations_active_idx
  on backup_operations(agent_id)
  where status in ('RUNNING','CANCEL_REQUESTED');

-- +goose Down
-- The old status check rejects rows still in LOST, failing closed.
drop index backup_operations_active_idx;
alter table backup_operations drop constraint backup_operations_status_check,
  add constraint backup_operations_status_check check (status in ('QUEUED','RUNNING','SUCCEEDED','SUCCEEDED_WITH_WARNINGS','FAILED','CANCEL_REQUESTED','CANCELED','MISSED','CONCURRENCY_SKIPPED'));
alter table agents drop column boot_changed_at;
