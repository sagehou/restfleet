#!/bin/sh
set -eu

app_password=$(tr -d '\r\n' < /run/secrets/postgres-app-password)
if [ -z "$app_password" ]; then
	echo "application database password is empty" >&2
	exit 1
fi

RESTFLEET_APP_PASSWORD=$app_password
export RESTFLEET_APP_PASSWORD
psql -v ON_ERROR_STOP=1 \
	--username "$POSTGRES_USER" \
	--dbname "$POSTGRES_DB" <<-'SQL'
	\getenv app_password RESTFLEET_APP_PASSWORD
	CREATE ROLE restfleet_app LOGIN PASSWORD :'app_password';
	REVOKE CONNECT ON DATABASE restfleet FROM PUBLIC;
	GRANT CONNECT ON DATABASE restfleet TO restfleet_migrator, restfleet_app;
	SQL
unset app_password RESTFLEET_APP_PASSWORD
