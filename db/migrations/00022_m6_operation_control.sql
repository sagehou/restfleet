-- +goose Up
alter table backup_operations drop constraint if exists backup_operations_no_retry;
alter table backup_operations drop constraint backup_operations_status_check,
  add constraint backup_operations_status_check check (status in ('QUEUED','RUNNING','SUCCEEDED','SUCCEEDED_WITH_WARNINGS','FAILED','CANCEL_REQUESTED','CANCELED','MISSED','CONCURRENCY_SKIPPED'));
alter table backup_operations
  add column parent_operation_id uuid references backup_operations(id),
  add column root_operation_id uuid references backup_operations(id),
  add column cancel_requested_at timestamptz,
  add column cancel_reason text not null default '';
create index backup_operations_parent_idx on backup_operations(parent_operation_id) where parent_operation_id is not null;

-- +goose Down
-- The old status check rejects rows still in CANCEL_REQUESTED, failing closed.
drop index backup_operations_parent_idx;
alter table backup_operations
  drop column parent_operation_id,
  drop column root_operation_id,
  drop column cancel_requested_at,
  drop column cancel_reason;
alter table backup_operations drop constraint backup_operations_status_check,
  add constraint backup_operations_status_check check (status in ('QUEUED','RUNNING','SUCCEEDED','SUCCEEDED_WITH_WARNINGS','FAILED','CANCELED','MISSED','CONCURRENCY_SKIPPED'));
