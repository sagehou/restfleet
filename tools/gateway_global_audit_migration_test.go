package tools

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestGlobalAuditMigrationPreservesHistoryAndRestrictsRole(t *testing.T) {
	database := os.Getenv("RESTFLEET_TEST_DATABASE_URL")
	if database == "" { t.Skip("Actions supplies PostgreSQL") }
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, database)
	if err != nil { t.Fatal(err) }
	defer conn.Close(ctx)
	tx, err := conn.Begin(ctx)
	if err != nil { t.Fatal(err) }
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `create schema restfleet_global_audit_test; set local search_path=restfleet_global_audit_test;
	 do $$ begin if not exists(select 1 from pg_roles where rolname='restfleet_app') then create role restfleet_app; end if; end $$;`); err != nil { t.Fatal(err) }
	raw, err := os.ReadFile("../db/migrations/00015_m4_gateway_global_audit.sql")
	if err != nil { t.Fatal(err) }
	up, down, ok := strings.Cut(string(raw), "-- +goose Down")
	if !ok { t.Fatal("migration directions") }
	for _, sql := range []string{up, down, up} {
		if _, err = tx.Exec(ctx, sql); err != nil { t.Fatal(err) }
	}
	if _, err = tx.Exec(ctx, `insert into gateway_audit_origins(id,runtime_id,public_key)
	 values('0198f1da-2c57-7d3b-9c92-6e2f05293641','0198f1da-2c57-7d3b-9c92-6e2f05293642',decode(repeat('08',32),'hex'));savepoint protected;`); err != nil { t.Fatal(err) }
	for _, sql := range []string{down,
		"update gateway_audit_origins set public_key='x'::bytea",
		"update gateway_audit_origins set runtime_id='00000000-0000-0000-0000-000000000000'",
		"update gateway_audit_origins set closed_at=created_at-interval '1 second'",
		"insert into gateway_audit_origins select * from gateway_audit_origins",
	} {
		if _, err = tx.Exec(ctx, sql); err == nil { t.Fatal("unsafe origin schema accepted") }
		if _, err = tx.Exec(ctx, "rollback to savepoint protected"); err != nil { t.Fatal(err) }
	}
	if _, err = tx.Exec(ctx, `insert into gateway_audit_records(origin_id,runtime_id,sequence,record_id,wire_hash,occurred_at)
	 select id,runtime_id,1,'0198f1da-2c57-7d3b-9c92-6e2f05293643',repeat('a',64),clock_timestamp() from gateway_audit_origins;savepoint recorded;`); err != nil { t.Fatal(err) }
	for _, sql := range []string{down, "update gateway_audit_records set sequence=0", "update gateway_audit_records set wire_hash=repeat('A',64)",
		"update gateway_audit_records set runtime_id='0198f1da-2c57-7d3b-9c92-6e2f05293644'", "insert into gateway_audit_records select * from gateway_audit_records"} {
		if _, err = tx.Exec(ctx, sql); err == nil { t.Fatal("unsafe record schema accepted") }
		if _, err = tx.Exec(ctx, "rollback to savepoint recorded"); err != nil { t.Fatal(err) }
	}
	var read, insert, update, del, rekey, seal bool
	if err = tx.QueryRow(ctx, `select has_table_privilege('restfleet_app','gateway_audit_records','SELECT'),
	 has_table_privilege('restfleet_app','gateway_audit_records','INSERT'),has_table_privilege('restfleet_app','gateway_audit_records','UPDATE'),
	 has_table_privilege('restfleet_app','gateway_audit_records','DELETE'),has_column_privilege('restfleet_app','gateway_audit_origins','public_key','UPDATE'),
	 has_column_privilege('restfleet_app','gateway_audit_origins','closed_at','UPDATE')`).Scan(&read, &insert, &update, &del, &rekey, &seal); err != nil || !read || !insert || update || del || rekey || !seal {
		t.Fatal("incorrect global audit role privileges")
	}
}
