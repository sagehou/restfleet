#!/bin/sh
set -eu

database_url_file=${RESTFLEET_DATABASE_URL_FILE:?RESTFLEET_DATABASE_URL_FILE is required}
if [ ! -r "$database_url_file" ]; then
	echo "database URL file is not readable" >&2
	exit 1
fi

GOOSE_DBSTRING=$(tr -d '\r\n' < "$database_url_file")
if [ -z "$GOOSE_DBSTRING" ]; then
	echo "database URL file is empty" >&2
	exit 1
fi
export GOOSE_DBSTRING

exec /usr/local/bin/goose "$@"
