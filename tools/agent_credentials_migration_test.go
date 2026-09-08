package tools

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestAgentCredentialMigrationRoundTripAndHistoryGuard(t *testing.T) {
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
	_, err = tx.Exec(ctx, `create schema restfleet_agent_credential_migration_test;set local search_path=restfleet_agent_credential_migration_test;
		create table agents(id uuid primary key);create table repositories(id uuid primary key);create table secrets(id uuid primary key);
		insert into agents values('0198f1da-2c57-7d3b-9c92-6e2f05293641');insert into repositories select id from agents;
		insert into secrets values('0198f1da-2c57-7d3b-9c92-6e2f05293642'),('0198f1da-2c57-7d3b-9c92-6e2f05293643');`)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("../db/migrations/00010_m4_agent_repository_credentials.sql")
	if err != nil {
		t.Fatal(err)
	}
	up, down, ok := strings.Cut(string(raw), "-- +goose Down")
	if !ok {
		t.Fatal("migration directions missing")
	}
	for _, sql := range []string{up, down, up} {
		if _, err := tx.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	_, err = tx.Exec(ctx, `insert into repository_agent_deliveries(agent_id,id,repository_id,revision,configuration_hash,gateway_secret_ref,restic_secret_ref,created_at)
		values('0198f1da-2c57-7d3b-9c92-6e2f05293641','0198f1da-2c57-7d3b-9c92-6e2f05293644','0198f1da-2c57-7d3b-9c92-6e2f05293641',1,repeat('a',64),
		'0198f1da-2c57-7d3b-9c92-6e2f05293642','0198f1da-2c57-7d3b-9c92-6e2f05293643',clock_timestamp());savepoint protected;`)
	if err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{down, "update repository_agent_deliveries set revision=0", "update repository_agent_deliveries set restic_secret_ref=gateway_secret_ref"} {
		if _, err := tx.Exec(ctx, sql); err == nil {
			t.Fatal("unsafe migration/state accepted")
		}
		if _, err := tx.Exec(ctx, "rollback to savepoint protected"); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err := tx.QueryRow(ctx, "select count(*) from repository_agent_deliveries").Scan(&count); err != nil || count != 1 {
		t.Fatal("delivery history lost")
	}
}
