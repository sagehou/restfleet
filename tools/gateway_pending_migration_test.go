package tools

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestGatewayPendingMigrationRetentionAndPrivileges(t *testing.T) {
	database := os.Getenv("RESTFLEET_TEST_DATABASE_URL")
	if database == "" {
		t.Skip("CI supplies PostgreSQL")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, database)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `create schema restfleet_pending_test; set local search_path=restfleet_pending_test;
	 create table gateway_backup_admissions(id uuid primary key);
	 insert into gateway_backup_admissions values('0198f1da-2c57-7d3b-9c92-6e2f05293641');`); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("../db/migrations/00013_m4_gateway_pending.sql")
	if err != nil {
		t.Fatal(err)
	}
	up, down, ok := strings.Cut(string(raw), "-- +goose Down")
	if !ok {
		t.Fatal("migration directions")
	}
	for _, sql := range []string{up, down, up} {
		if _, err = tx.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = tx.Exec(ctx, `insert into gateway_pending_origins(runtime_id,admission_id,public_key,initial_secret_revision)
	 select '0198f1da-2c57-7d3b-9c92-6e2f05293642',id,decode(repeat('08',32),'hex'),1 from gateway_backup_admissions;
	 savepoint protected;`); err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{down, "update gateway_pending_origins set public_key='x'::bytea", "update gateway_pending_origins set initial_secret_revision=0", "update gateway_pending_origins set closed_at=created_at-interval '1 second'", "update gateway_pending_origins set runtime_id='00000000-0000-0000-0000-000000000000'"} {
		if _, err = tx.Exec(ctx, sql); err == nil {
			t.Fatal("unsafe origin schema accepted")
		}
		if _, err = tx.Exec(ctx, "rollback to savepoint protected"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = tx.Exec(ctx, `insert into gateway_pending_records(admission_id,runtime_id,sequence,record_id,wire_hash,authorization_revision,kind,occurred_at)
	 select admission_id,runtime_id,1,'0198f1da-2c57-7d3b-9c92-6e2f05293643',repeat('a',64),1,'audit',clock_timestamp() from gateway_pending_origins;
	 savepoint recorded;`); err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{down, "update gateway_pending_records set sequence=0", "update gateway_pending_records set kind='secret-canary'", "update gateway_pending_records set wire_hash=repeat('A',64)", "update gateway_pending_records set authorization_revision=0", "insert into gateway_pending_records select * from gateway_pending_records"} {
		if _, err = tx.Exec(ctx, sql); err == nil {
			t.Fatal("unsafe record schema accepted")
		}
		if _, err = tx.Exec(ctx, "rollback to savepoint recorded"); err != nil {
			t.Fatal(err)
		}
	}
	var roleExists bool
	if err = tx.QueryRow(ctx, "select exists(select 1 from pg_roles where rolname='restfleet_app')").Scan(&roleExists); err != nil {
		t.Fatal(err)
	}
	if roleExists {
		var read, insert, update, del, rekey, seal bool
		if err = tx.QueryRow(ctx, `select has_table_privilege('restfleet_app','gateway_pending_records','SELECT'),
		 has_table_privilege('restfleet_app','gateway_pending_records','INSERT'),has_table_privilege('restfleet_app','gateway_pending_records','UPDATE'),
		 has_table_privilege('restfleet_app','gateway_pending_records','DELETE'),has_column_privilege('restfleet_app','gateway_pending_origins','public_key','UPDATE'),
		 has_column_privilege('restfleet_app','gateway_pending_origins','closed_at','UPDATE')`).Scan(&read, &insert, &update, &del, &rekey, &seal); err != nil || !read || !insert || update || del || rekey || !seal {
			t.Fatal("incorrect persistence privileges")
		}
	}
}
