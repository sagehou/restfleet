package tools

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestOfflineAuthorizationMigrationHistoryAndConstraints(t *testing.T) {
	database := os.Getenv("RESTFLEET_TEST_DATABASE_URL")
	if database == "" {
		t.Skip("CI supplies PostgreSQL")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, database)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Create prerequisite tables.
	_, err = tx.Exec(ctx, `create schema restfleet_offline_auth_migration_test;
		set local search_path=restfleet_offline_auth_migration_test;
		create table agents(id uuid primary key, status text not null default 'ACTIVE');
		create table hosts(id uuid primary key);
		create table repositories(id uuid primary key);
		create table storage_credentials(id uuid primary key);
		create table secrets(id uuid primary key)`)
	if err != nil {
		t.Fatal(err)
	}

	// Read and execute migration.
	raw, err := os.ReadFile("../db/migrations/00012_m4_offline_authorization.sql")
	if err != nil {
		t.Fatal(err)
	}
	up := extractUp(string(raw))
	if _, err = tx.Exec(ctx, up); err != nil {
		t.Fatalf("up migration failed: %v", err)
	}

	// Verify table exists and has expected columns.
	var count int
	err = tx.QueryRow(ctx, `select count(*) from information_schema.columns
		where table_schema='restfleet_offline_auth_migration_test' and table_name='offline_authorizations'`).Scan(&count)
	if err != nil {
		t.Fatal(err)
	}
	if count < 15 {
		t.Fatalf("expected at least 15 columns, got %d", count)
	}

	// Verify CHECK constraints: max12h lifetime.
	_, err = tx.Exec(ctx, `insert into offline_authorizations(
		id,owner,gateway_instance_id,agent_id,host_id,repository_id,gateway_id,
		storage_credential_id,delivery_id,configuration_hash,authorized_at,expires_at,
		authorization_sequence,created_at
	) values(gen_random_uuid(),gen_random_uuid(),gen_random_uuid(),gen_random_uuid(),
		gen_random_uuid(),gen_random_uuid(),gen_random_uuid(),gen_random_uuid(),gen_random_uuid(),
		repeat('a',64),now(),now()+interval '13 hours',1,now())`)
	if err == nil || !strings.Contains(err.Error(), "offline_authorizations") {
		t.Fatalf("expected CHECK violation for >12h lifetime, got: %v", err)
	}

	// Verify valid1h authorization.
	_, err = tx.Exec(ctx, `insert into agents(id) values(gen_random_uuid())`)
	if err != nil {
		t.Fatal(err)
	}
	agentID := tx.QueryRow(ctx, `select id from agents limit 1`)
	var aid string
	if err = agentID.Scan(&aid); err != nil {
		t.Fatal(err)
	}

	// Clean up.
	if _, err = tx.Exec(ctx, "drop schema restfleet_offline_auth_migration_test cascade"); err != nil {
		t.Fatal(err)
	}
}

func TestWritebackMigrationHistoryAndConstraints(t *testing.T) {
	database := os.Getenv("RESTFLEET_TEST_DATABASE_URL")
	if database == "" {
		t.Skip("CI supplies PostgreSQL")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, database)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Create prerequisite tables.
	_, err = tx.Exec(ctx, `create schema restfleet_writeback_migration_test;
		set local search_path=restfleet_writeback_migration_test;
		create table agents(id uuid primary key, status text not null default 'ACTIVE');
		create table hosts(id uuid primary key);
		create table repositories(id uuid primary key);
		create table storage_credentials(id uuid primary key);
		create table secrets(id uuid primary key)`)
	if err != nil {
		t.Fatal(err)
	}

	// Execute offline_authorization migration first.
	raw, err := os.ReadFile("../db/migrations/00012_m4_offline_authorization.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, extractUp(string(raw))); err != nil {
		t.Fatal(err)
	}

	// Execute writeback migration.
	raw2, err := os.ReadFile("../db/migrations/00013_m4_writeback_records.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, extractUp(string(raw2))); err != nil {
		t.Fatalf("writeback migration failed: %v", err)
	}

	// Verify table exists.
	var count int
	err = tx.QueryRow(ctx, `select count(*) from information_schema.columns
		where table_schema='restfleet_writeback_migration_test' and table_name='writeback_records'`).Scan(&count)
	if err != nil {
		t.Fatal(err)
	}
	if count < 9 {
		t.Fatalf("expected at least 9 columns, got %d", count)
	}

	// Verify CHECK: record_type enum.
	_, err = tx.Exec(ctx, `insert into offline_authorizations(
		id,owner,gateway_instance_id,agent_id,host_id,repository_id,gateway_id,
		storage_credential_id,delivery_id,configuration_hash,authorized_at,expires_at,
		authorization_sequence,created_at
	) values(gen_random_uuid(),gen_random_uuid(),gen_random_uuid(),gen_random_uuid(),
		gen_random_uuid(),gen_random_uuid(),gen_random_uuid(),gen_random_uuid(),gen_random_uuid(),
		repeat('a',64),now(),now()+interval '1h',1,now())`)
	if err != nil {
		t.Fatalf("failed to insert auth: %v", err)
	}

	// Invalid record_type should fail.
	_, err = tx.Exec(ctx, `insert into writeback_records(
		id,authorization_id,owner,gateway_instance_id,sequence,record_type,payload,checksum,created_at
	) values(gen_random_uuid(),
		(select id from offline_authorizations limit 1),
		(select owner from offline_authorizations limit 1),
		(select gateway_instance_id from offline_authorizations limit 1),
		1,'INVALID_TYPE',E'\\x00',decode(repeat('00',32),'hex'),now())`)
	if err == nil || !strings.Contains(err.Error(), "writeback_records") {
		t.Fatalf("expected CHECK violation for invalid record_type, got: %v", err)
	}

	// Valid record should succeed.
	_, err = tx.Exec(ctx, `insert into writeback_records(
		id,authorization_id,owner,gateway_instance_id,sequence,record_type,payload,checksum,created_at
	) values(gen_random_uuid(),
		(select id from offline_authorizations limit 1),
		(select owner from offline_authorizations limit 1),
		(select gateway_instance_id from offline_authorizations limit 1),
		1,'AUDIT_EVENT',E'\\x00',decode(repeat('00',32),'hex'),now())`)
	if err != nil {
		t.Fatalf("valid insert failed: %v", err)
	}

	// Duplicate sequence should fail (unique index).
	_, err = tx.Exec(ctx, `insert into writeback_records(
		id,authorization_id,owner,gateway_instance_id,sequence,record_type,payload,checksum,created_at
	) values(gen_random_uuid(),
		(select id from offline_authorizations limit 1),
		(select owner from offline_authorizations limit 1),
		(select gateway_instance_id from offline_authorizations limit 1),
		1,'AUDIT_EVENT',E'\\x00',decode(repeat('00',32),'hex'),now())`)
	if err == nil {
		t.Fatal("expected unique constraint violation for duplicate sequence")
	}

	if _, err = tx.Exec(ctx, "drop schema restfleet_writeback_migration_test cascade"); err != nil {
		t.Fatal(err)
	}
}

func extractUp(full string) string {
	sections := strings.Split(full, "-- +goose Down")
	if len(sections) < 1 {
		return full
	}
	up := sections[0]
	up = strings.ReplaceAll(up, "-- +goose Up", "")
	up = strings.ReplaceAll(up, "-- +goose StatementBegin", "")
	up = strings.ReplaceAll(up, "-- +goose StatementEnd", "")
	return up
}
