-- +goose Up
-- Dispatch is claimed before the JobDispatch is written to the stream so a
-- concurrent cancel can either close a never-dispatched operation or ask the
-- Agent to cancel one that may already be on the wire.
alter table backup_operations add column dispatched_at timestamptz;
-- Existing non-terminal rows may already have been dispatched before this
-- migration. Treat them as dispatched so cancel is delivered to the Agent and
-- a still-unreported run is not silently closed while it may be executing.
update backup_operations
  set dispatched_at = coalesce(started_at, created_at)
  where finished_at is null;
create index backup_operations_cancel_idx
  on backup_operations(agent_id, cancel_requested_at)
  where status = 'CANCEL_REQUESTED';

-- +goose Down
drop index backup_operations_cancel_idx;
alter table backup_operations drop column dispatched_at;
