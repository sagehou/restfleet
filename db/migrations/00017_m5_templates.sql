-- +goose Up
create table templates (
  id uuid primary key,
  name text not null check (char_length(name) between 1 and 128),
  description text not null default '' check (char_length(description) <= 1024),
  current_revision bigint not null check (current_revision > 0),
  archived_at timestamptz,
  created_at timestamptz not null,
  updated_at timestamptz not null
);
-- Active templates stay uniquely named; archived rows keep their history.
create unique index templates_name_idx on templates(lower(name)) where archived_at is null;

create table template_revisions (
  template_id uuid not null references templates(id),
  revision bigint not null check (revision > 0),
  config_json jsonb not null,
  config_hash text not null check (config_hash ~ '^[0-9a-f]{64}$'),
  created_by uuid not null references users(id),
  created_at timestamptz not null,
  primary key (template_id, revision)
);

revoke all on templates, template_revisions from public;
-- +goose StatementBegin
do $restfleet_permissions$
begin
  if exists (select 1 from pg_roles where rolname = 'restfleet_app') then
    execute 'grant select, insert, update on templates to restfleet_app';
    execute 'grant select, insert on template_revisions to restfleet_app';
  end if;
end
$restfleet_permissions$;
-- +goose StatementEnd

-- +goose Down
drop table template_revisions;
drop table templates;
