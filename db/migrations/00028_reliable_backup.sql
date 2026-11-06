-- +goose Up
alter table backup_operations add column attempt integer not null default 1 check (attempt between 1 and 10000);
alter table backup_operations add column summary_json jsonb;
alter table plans add column last_result_at timestamptz;
update plans p set last_result_at=o.finished_at from backup_operations o where o.id=p.last_operation_id;

-- +goose Down
alter table backup_operations drop column attempt;
alter table backup_operations drop column summary_json;
alter table plans drop column last_result_at;
