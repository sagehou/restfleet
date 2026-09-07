-- +goose Up
alter table storage_credentials drop constraint storage_credentials_provider_check;
alter table storage_credentials add constraint storage_credentials_provider_check
  check (provider in ('RCLONE_ONEDRIVE','RCLONE_GDRIVE','RCLONE_WEBDAV'));

-- +goose Down
-- Fail instead of discarding or mislabelling any newer provider's credentials.
alter table storage_credentials drop constraint storage_credentials_provider_check;
alter table storage_credentials add constraint storage_credentials_provider_check
  check (provider = 'RCLONE_ONEDRIVE');
