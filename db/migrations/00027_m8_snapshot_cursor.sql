-- +goose Up
alter table snapshot_list_results
  add column cursor text not null default '',
  add column next_cursor text not null default '';
drop index snapshot_list_cache_idx;
create index snapshot_list_cache_idx
  on snapshot_list_results(repository_id, snapshot_id, path, cursor, expires_at desc);

-- +goose Down
drop index snapshot_list_cache_idx;
create index snapshot_list_cache_idx
  on snapshot_list_results(repository_id, snapshot_id, path, expires_at desc);
alter table snapshot_list_results drop column cursor, drop column next_cursor;
