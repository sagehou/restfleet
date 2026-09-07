package tools

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// A transaction-local table exercises upgrade/down without touching application data.
func TestProviderMigrationPreservesLegacy(t *testing.T) {
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
	_, err = tx.Exec(ctx, `create temporary table storage_credentials (
		provider text not null constraint storage_credentials_provider_check check (provider='RCLONE_ONEDRIVE'),
		ciphertext bytea not null, revision bigint not null
	) on commit drop`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, "insert into storage_credentials values ('RCLONE_ONEDRIVE',$1,7)", []byte{1, 2, 3, 4}); err != nil {
		t.Fatal(err)
	}
	migration, err := os.ReadFile("../db/migrations/00008_m4_rclone_backends.sql")
	if err != nil {
		t.Fatal(err)
	}
	up, down, ok := strings.Cut(string(migration), "-- +goose Down")
	if !ok {
		t.Fatal("migration directions missing")
	}
	if _, err = tx.Exec(ctx, up); err != nil {
		t.Fatal(err)
	}
	var provider string
	var ciphertext []byte
	var revision int64
	if err = tx.QueryRow(ctx, "select provider,ciphertext,revision from storage_credentials").Scan(&provider, &ciphertext, &revision); err != nil {
		t.Fatal(err)
	}
	if provider != "RCLONE_ONEDRIVE" || !bytes.Equal(ciphertext, []byte{1, 2, 3, 4}) || revision != 7 {
		t.Fatal("legacy credential rewritten")
	}
	if _, err = tx.Exec(ctx, down); err != nil {
		t.Fatal("legacy-only rollback failed")
	}
	if _, err = tx.Exec(ctx, up); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, "insert into storage_credentials values ('RCLONE_GDRIVE',$1,1),('RCLONE_WEBDAV',$1,1)", []byte{5}); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, "savepoint before_down"); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, down); err == nil {
		t.Fatal("lossy downgrade accepted")
	}
	if _, err = tx.Exec(ctx, "rollback to savepoint before_down"); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = tx.QueryRow(ctx, "select count(*) from storage_credentials").Scan(&count); err != nil || count != 3 {
		t.Fatal("failed downgrade changed records")
	}
}
