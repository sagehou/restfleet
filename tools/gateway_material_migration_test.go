package tools

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestGatewayMaterialMigrationRetentionAndPrivileges(t *testing.T) {
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
	if _, err = tx.Exec(ctx, `create schema restfleet_material_test;set local search_path=restfleet_material_test;
	create table gateway_pending_origins(admission_id uuid,runtime_id uuid,primary key(admission_id,runtime_id));
	insert into gateway_pending_origins values('0198f1da-2c57-7d3b-9c92-6e2f05293641','0198f1da-2c57-7d3b-9c92-6e2f05293642');`); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("../db/migrations/00014_m4_gateway_material_delivery.sql")
	if err != nil {
		t.Fatal(err)
	}
	up, down, ok := strings.Cut(string(raw), "-- +goose Down")
	if !ok {
		t.Fatal("directions")
	}
	for _, sql := range []string{up, down, up} {
		if _, err = tx.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = tx.Exec(ctx, `insert into gateway_material_deliveries(admission_id,runtime_id,challenge_hash,authorization_revision,secret_revision)
	select admission_id,runtime_id,repeat('a',64),1,1 from gateway_pending_origins;savepoint protected;`); err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{down, "insert into gateway_material_deliveries select * from gateway_material_deliveries", "update gateway_material_deliveries set authorization_revision=0", "update gateway_material_deliveries set secret_revision=0", "update gateway_material_deliveries set challenge_hash=repeat('A',64)", "update gateway_material_deliveries set runtime_id='00000000-0000-0000-0000-000000000000'"} {
		if _, err = tx.Exec(ctx, sql); err == nil {
			t.Fatal("unsafe migration accepted")
		}
		if _, err = tx.Exec(ctx, "rollback to savepoint protected"); err != nil {
			t.Fatal(err)
		}
	}
	var exists bool
	if err = tx.QueryRow(ctx, "select exists(select 1 from pg_roles where rolname='restfleet_app')").Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		var read, insert, update, del bool
		if err = tx.QueryRow(ctx, `select has_table_privilege('restfleet_app','gateway_material_deliveries','SELECT'),has_table_privilege('restfleet_app','gateway_material_deliveries','INSERT'),has_table_privilege('restfleet_app','gateway_material_deliveries','UPDATE'),has_table_privilege('restfleet_app','gateway_material_deliveries','DELETE')`).Scan(&read, &insert, &update, &del); err != nil || !read || !insert || update || del {
			t.Fatal("delivery runtime privileges")
		}
	}
}
