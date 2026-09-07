package tools

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestInitializationMigrationRoundTripAndHistoryGuard(t *testing.T) {
	database := os.Getenv("RESTFLEET_TEST_DATABASE_URL")
	if database == "" {
		t.Skip("CI supplies isolated PostgreSQL")
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
	// All DDL and grants are transaction-local in a separate schema. The real
	// migrated application's tables, data and privileges are never touched.
	_, err = tx.Exec(ctx, `create schema restfleet_initialize_migration_test;
		set local search_path=restfleet_initialize_migration_test;
		create table repositories(id uuid primary key, format_version integer, revision bigint, updated_at timestamptz);
		create table operations(id uuid primary key, type text check(type='CREDENTIAL_TEST'),error_code text check(error_code in ('','WORKER_LOST')),finished_at timestamptz);
		create table jobs(queue text check(queue='CREDENTIAL_TEST'));
		insert into repositories values('0198f1da-2c57-7d3b-9c92-6e2f05293647',null,7,'2026-09-07T00:00:00Z');`)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("../db/migrations/00009_m4_repository_initialization.sql")
	if err != nil {
		t.Fatal(err)
	}
	up, down, ok := strings.Cut(string(raw), "-- +goose Down")
	if !ok {
		t.Fatal("missing migration directions")
	}
	if _, err = tx.Exec(ctx, up); err != nil {
		t.Fatal(err)
	}
	var revision int
	var initialized bool
	if err = tx.QueryRow(ctx, "select revision,initialized_at is not null from repositories").Scan(&revision, &initialized); err != nil || revision != 7 || initialized {
		t.Fatal("legacy repository changed")
	}
	if _, err = tx.Exec(ctx, down); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, up); err != nil {
		t.Fatal(err)
	}
	_, err = tx.Exec(ctx, `insert into operations(id,type,error_code,repository_id)
		values('0198f1da-2c57-7d3b-9c92-6e2f05293648','REPOSITORY_INITIALIZE','','0198f1da-2c57-7d3b-9c92-6e2f05293647');
		update repositories set last_initialize_operation_id='0198f1da-2c57-7d3b-9c92-6e2f05293648';
		savepoint before_down;`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, down); err == nil {
		t.Fatal("downgrade dropped initialization history")
	}
	if _, err = tx.Exec(ctx, "rollback to savepoint before_down"); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = tx.QueryRow(ctx, "select count(*) from operations where type='REPOSITORY_INITIALIZE'").Scan(&count); err != nil || count != 1 {
		t.Fatal("history lost")
	}
	// SQL CHECK must reject NULL format as well as unknown versions, rather
	// than letting PostgreSQL's three-valued check semantics accept NULL.
	if _, err = tx.Exec(ctx, "savepoint before_invalid_result"); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, "update repositories set restic_id=repeat('a',64),initialized_at=clock_timestamp(),format_version=null"); err == nil {
		t.Fatal("unverified format accepted")
	}
	if _, err = tx.Exec(ctx, "rollback to savepoint before_invalid_result"); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, "update repositories set restic_id=repeat('a',64),initialized_at=clock_timestamp(),format_version=2"); err != nil {
		t.Fatal(err)
	}
}
