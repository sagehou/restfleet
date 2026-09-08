package tools

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestBackupAdmissionMigrationHistoryAndConstraints(t *testing.T) {
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
	_, err = tx.Exec(ctx, `create schema restfleet_backup_admission_migration_test;set local search_path=restfleet_backup_admission_migration_test;
		create table agents(id uuid primary key);create table hosts(id uuid primary key);create table repositories(id uuid primary key);
		create table storage_credentials(id uuid primary key);create table secrets(id uuid primary key);
		insert into agents values('0198f1da-2c57-7d3b-9c92-6e2f05293641');insert into hosts select id from agents;
		insert into repositories select id from agents;insert into storage_credentials select id from agents;
		insert into secrets values('0198f1da-2c57-7d3b-9c92-6e2f05293642'),('0198f1da-2c57-7d3b-9c92-6e2f05293643');`)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("../db/migrations/00011_m4_backup_admissions.sql")
	if err != nil {
		t.Fatal(err)
	}
	up, down, ok := strings.Cut(string(raw), "-- +goose Down")
	if !ok {
		t.Fatal("missing directions")
	}
	for _, sql := range []string{up, down, up} {
		if _, err := tx.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	_, err = tx.Exec(ctx, `insert into gateway_backup_admissions(id,owner,agent_id,host_id,repository_id,gateway_id,storage_credential_id,delivery_id,gateway_secret_ref,restic_secret_ref,configuration_hash,created_at,expires_at)
		select '0198f1da-2c57-7d3b-9c92-6e2f05293644',id,id,id,id,id,id,id,'0198f1da-2c57-7d3b-9c92-6e2f05293642','0198f1da-2c57-7d3b-9c92-6e2f05293643',repeat('a',64),clock_timestamp(),clock_timestamp()+interval '1 hour' from agents;
		savepoint protected;`)
	if err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{down,
		"update gateway_backup_admissions set expires_at=created_at+interval '25 hours'",
		"update gateway_backup_admissions set expires_at=created_at",
		"update gateway_backup_admissions set released_at=created_at-interval '1 second'",
		"update gateway_backup_admissions set gateway_secret_ref=restic_secret_ref",
		"update gateway_backup_admissions set configuration_hash='invalid'",
		"update gateway_backup_admissions set agent_id='0198f1da-2c57-7d3b-9c92-6e2f05293649'",
		`insert into gateway_backup_admissions select '0198f1da-2c57-7d3b-9c92-6e2f05293645',owner,agent_id,host_id,repository_id,gateway_id,storage_credential_id,delivery_id,gateway_secret_ref,restic_secret_ref,configuration_hash,created_at,expires_at,released_at from gateway_backup_admissions`,
	} {
		if _, err := tx.Exec(ctx, sql); err == nil {
			t.Fatal("unsafe schema transition accepted")
		}
		if _, err := tx.Exec(ctx, "rollback to savepoint protected"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tx.Exec(ctx, "update gateway_backup_admissions set released_at=clock_timestamp();savepoint released"); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, down); err == nil {
		t.Fatal("released history discarded")
	}
	if _, err := tx.Exec(ctx, "rollback to savepoint released"); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := tx.QueryRow(ctx, "select count(*) from gateway_backup_admissions").Scan(&count); err != nil || count != 1 {
		t.Fatal("history lost")
	}
	var roleExists bool
	if err := tx.QueryRow(ctx, "select exists(select 1 from pg_roles where rolname='restfleet_app')").Scan(&roleExists); err != nil {
		t.Fatal(err)
	}
	if roleExists {
		var canDelete, canReassign, canRelease bool
		if err := tx.QueryRow(ctx, `select has_table_privilege('restfleet_app','gateway_backup_admissions','DELETE'),has_column_privilege('restfleet_app','gateway_backup_admissions','owner','UPDATE'),has_column_privilege('restfleet_app','gateway_backup_admissions','released_at','UPDATE')`).Scan(&canDelete, &canReassign, &canRelease); err != nil || canDelete || canReassign || !canRelease {
			t.Fatal("excessive or missing runtime privileges")
		}
	}
}
