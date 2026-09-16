package tools

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestGatewayDecisionsMigrationHistoryAndConstraints(t *testing.T) {
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
	_, err = tx.Exec(ctx, `create schema restfleet_gateway_decisions_test;set local search_path=restfleet_gateway_decisions_test;
		create table gateway_backup_admissions(id uuid primary key);
		insert into gateway_backup_admissions values('0198f1da-2c57-7d3b-9c92-6e2f05293641');`)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("../db/migrations/00012_m4_gateway_decisions.sql")
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
	_, err = tx.Exec(ctx, `insert into gateway_authorization_decisions(admission_id,revision,request_id,runtime_id,lifetime_seconds,issued_at,expires_at,revoked)
		select id,1,'0198f1da-2c57-7d3b-9c92-6e2f05293642','0198f1da-2c57-7d3b-9c92-6e2f05293643',3600,'2026-09-16 00:00:00+00','2026-09-16 01:00:00+00',false from gateway_backup_admissions;
		savepoint protected;`)
	if err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{
		down, "update gateway_authorization_decisions set revision=0",
		"update gateway_authorization_decisions set expires_at=issued_at",
		"update gateway_authorization_decisions set lifetime_seconds=43201",
		"update gateway_authorization_decisions set revoked=true",
		"update gateway_authorization_decisions set expires_at=null",
		"update gateway_authorization_decisions set issued_at=issued_at+interval '0.1 seconds'",
		"update gateway_authorization_decisions set expires_at=expires_at+interval '1 second'",
		"update gateway_authorization_decisions set runtime_id='00000000-0000-0000-0000-000000000000'",
		"insert into gateway_authorization_decisions select * from gateway_authorization_decisions",
	} {
		if _, err := tx.Exec(ctx, sql); err == nil {
			t.Fatal("unsafe schema change accepted")
		}
		if _, err := tx.Exec(ctx, "rollback to savepoint protected"); err != nil {
			t.Fatal(err)
		}
	}
	_, err = tx.Exec(ctx, `insert into gateway_authorization_decisions
		select admission_id,2,'0198f1da-2c57-7d3b-9c92-6e2f05293644',runtime_id,0,issued_at,null,true from gateway_authorization_decisions;savepoint revoked;`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, down); err == nil {
		t.Fatal("revoked history discarded")
	}
	if _, err = tx.Exec(ctx, "rollback to savepoint revoked"); err != nil {
		t.Fatal(err)
	}
	var roleExists bool
	if err = tx.QueryRow(ctx, "select exists(select 1 from pg_roles where rolname='restfleet_app')").Scan(&roleExists); err != nil {
		t.Fatal(err)
	}
	if roleExists {
		var read, insert, update, del bool
		if err = tx.QueryRow(ctx, `select has_table_privilege('restfleet_app','gateway_authorization_decisions','SELECT'),
			has_table_privilege('restfleet_app','gateway_authorization_decisions','INSERT'),
			has_table_privilege('restfleet_app','gateway_authorization_decisions','UPDATE'),
			has_table_privilege('restfleet_app','gateway_authorization_decisions','DELETE')`).Scan(&read, &insert, &update, &del); err != nil || !read || !insert || update || del {
			t.Fatal("incorrect append-only privileges")
		}
	}
}
