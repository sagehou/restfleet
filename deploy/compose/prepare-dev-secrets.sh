#!/bin/sh
set -eu

compose_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
secret_dir="$compose_dir/secrets"
umask 077
mkdir -p "$secret_dir"

for name in \
	postgres-migrator-password \
	postgres-app-password \
	migrator-database-url \
	app-database-url \
	bootstrap-token \
	master-key \
	grpc-tls-cert \
	grpc-tls-key \
	server-ca-bundle
do
	if [ -e "$secret_dir/$name" ]; then
		echo "refusing to overwrite existing development secrets" >&2
		exit 1
	fi
done

if ! command -v openssl >/dev/null 2>&1; then
	echo "openssl is required to create development secrets" >&2
	exit 1
fi

migrator_password=$(openssl rand -hex 32)
app_password=$(openssl rand -hex 32)
bootstrap_token=$(openssl rand -hex 32)
master_key=$(openssl rand -base64 32)

printf '%s\n' "$migrator_password" > "$secret_dir/postgres-migrator-password"
printf '%s\n' "$app_password" > "$secret_dir/postgres-app-password"
printf 'postgres://restfleet_migrator:%s@postgres:5432/restfleet?sslmode=disable\n' "$migrator_password" > "$secret_dir/migrator-database-url"
printf 'postgres://restfleet_app:%s@postgres:5432/restfleet?sslmode=disable\n' "$app_password" > "$secret_dir/app-database-url"
printf '%s\n' "$bootstrap_token" > "$secret_dir/bootstrap-token"
printf '%s\n' "$master_key" > "$secret_dir/master-key"
openssl req -x509 -newkey ed25519 -nodes -days 3650 \
	-keyout "$secret_dir/grpc-tls-key" \
	-out "$secret_dir/grpc-tls-cert" \
	-subj "/CN=RestFleet Development" \
	-addext "subjectAltName=DNS:localhost,DNS:server,IP:127.0.0.1" \
	-addext "basicConstraints=critical,CA:TRUE" \
	-addext "keyUsage=critical,digitalSignature,keyCertSign" \
	-addext "extendedKeyUsage=serverAuth" >/dev/null 2>&1
cp "$secret_dir/grpc-tls-cert" "$secret_dir/server-ca-bundle"
chmod 0600 "$secret_dir"/*

unset migrator_password app_password bootstrap_token master_key
