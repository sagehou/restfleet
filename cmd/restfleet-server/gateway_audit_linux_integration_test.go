package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sagehou/restfleet/internal/domain"
	"github.com/sagehou/restfleet/internal/security"
	control "github.com/sagehou/restfleet/internal/server"
)

// Separate schema, with constraints/identity columns copied from the migrated
// tables. The command uses its actual file loader, runtime config and DB store;
// no services start and the other packages' integration resets cannot erase it.
func auditCommandFixture(t *testing.T) (*pgxpool.Pool, string, control.GatewayAuditRegistrationConfig) {
	t.Helper()
	databaseURL := os.Getenv("RESTFLEET_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("RESTFLEET_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	schema := "rf_audit_command_" + strings.ReplaceAll(uuid.Must(uuid.NewV7()).String(), "-", "")
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(ctx, "create schema "+quoted); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(ctx, "drop schema "+quoted+" cascade"); err != nil {
			t.Error("command fixture schema cleanup failed")
		}
	})
	for _, table := range []string{"goose_db_version", "audit_events", "gateway_audit_origins"} {
		if _, err := admin.Exec(ctx, "create table "+quoted+"."+pgx.Identifier{table}.Sanitize()+" (like public."+pgx.Identifier{table}.Sanitize()+" including all)"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := admin.Exec(ctx, "insert into "+quoted+".goose_db_version select * from public.goose_db_version"); err != nil {
		t.Fatal(err)
	}
	db, err := url.Parse(databaseURL)
	if err != nil || (db.Scheme != "postgres" && db.Scheme != "postgresql") {
		t.Fatal("command fixture requires a PostgreSQL URL")
	}
	query := db.Query()
	query.Set("search_path", schema)
	db.RawQuery = query.Encode()
	pool, err := pgxpool.New(ctx, db.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	for _, name := range []string{"RESTFLEET_DATABASE_URL", "RESTFLEET_BOOTSTRAP_TOKEN", "RESTFLEET_BOOTSTRAP_TOKEN_FILE", "RESTFLEET_MASTER_KEY",
		"RESTFLEET_GATEWAY_SIGNING_KEY", "RESTFLEET_GATEWAY_PENDING_KEY", "RESTFLEET_GATEWAY_REPLAY_SOCKET",
		"RESTFLEET_GATEWAY_REPLAY_PEER_UID", "RESTFLEET_GATEWAY_REPLAY_GROUP"} {
		value, present := os.LookupEnv(name)
		if os.Unsetenv(name) != nil {
			t.Fatal("environment reset")
		}
		t.Cleanup(func() {
			if present {
				_ = os.Setenv(name, value)
			} else {
				_ = os.Unsetenv(name)
			}
		})
	}
	dir := t.TempDir()
	if os.Chmod(dir, 0700) != nil {
		t.Fatal("command private directory")
	}
	write := func(name string, raw []byte) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if os.WriteFile(path, raw, 0600) != nil {
			t.Fatal("command protected fixture")
		}
		return path
	}
	t.Setenv("RESTFLEET_ENV", "test")
	t.Setenv("RESTFLEET_DATABASE_URL_FILE", write("database-url", []byte(db.String())))
	t.Setenv("RESTFLEET_MASTER_KEY_FILE", write("master-key", []byte(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{8}, 32)))))
	centralSeed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(centralSeed); err != nil {
		t.Fatal(err)
	}
	defer clear(centralSeed)
	t.Setenv("RESTFLEET_GATEWAY_SIGNING_KEY_FILE", write("signing.seed", []byte(base64.StdEncoding.EncodeToString(centralSeed))))
	pending := make([]byte, 32)
	if _, err := rand.Read(pending); err != nil {
		t.Fatal(err)
	}
	defer clear(pending)
	t.Setenv("RESTFLEET_GATEWAY_PENDING_KEY_FILE", write("pending.key", []byte(base64.StdEncoding.EncodeToString(pending))))
	ca, key, err := security.NewAgentCA(time.Now().UTC())
	clear(key)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("RESTFLEET_SERVER_CA_BUNDLE_FILE", write("ca.pem", ca.CertificatePEM()))
	t.Setenv("RESTFLEET_PUBLIC_URL", "https://control.example")
	t.Setenv("RESTFLEET_GATEWAY_PUBLIC_URL", "https://gateway.example")
	t.Setenv("RESTFLEET_GRPC_ENDPOINT", "control.example:443")
	t.Setenv("RESTFLEET_GRPC_SERVER_NAME", "control.example")
	// A one-shot command MUST NOT read server TLS keys or start a listener.
	t.Setenv("RESTFLEET_GRPC_TLS_CERT_FILE", filepath.Join(dir, "absent-server.crt"))
	t.Setenv("RESTFLEET_GRPC_TLS_KEY_FILE", filepath.Join(dir, "absent-server.key"))
	t.Setenv("RESTFLEET_SECURE_COOKIES", "true")
	public, source, err := ed25519.GenerateKey(rand.Reader)
	clear(source)
	if err != nil {
		t.Fatal(err)
	}
	s := control.GatewayAuditRegistrationConfig{Version: 1, AuditOrigin: domain.GatewayAuditBinding{
		OriginID: uuid.Must(uuid.NewV7()), RuntimeID: uuid.Must(uuid.NewV7())}, SourcePublic: public}
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return pool, write("registration.json", raw), s
}

type unavailableAuditOutput struct{}

func (unavailableAuditOutput) Write([]byte) (int, error) {
	return 0, errors.New("private-output-canary")
}

func TestGatewayAuditCommandCommitsPublicRegistrationAndPreservesUncertainOutput(t *testing.T) {
	pool, path, s := auditCommandFixture(t)
	args := []string{"gateway-audit-register", "--config-file", path}
	if err := runGatewayCommand(args, unavailableAuditOutput{}); err != control.ErrGatewayAuditRegistration {
		t.Fatal("output failure accepted or raw error returned")
	}
	assertCounts := func(origins, audits int) {
		t.Helper()
		var registered, events int
		if err := pool.QueryRow(context.Background(), "select (select count(*) from gateway_audit_origins),(select count(*) from audit_events)").Scan(&registered, &events); err != nil || registered != origins || events != audits {
			t.Fatalf("registration effects origins=%d audits=%d: %v", registered, events, err)
		}
	}
	assertCounts(1, 1)
	var reference []byte
	for range 2 {
		var output bytes.Buffer
		if err := runGatewayCommand(args, &output); err != nil {
			t.Fatal("exact registration retry failed")
		}
		var result control.GatewayAuditRegistration
		var fields map[string]json.RawMessage
		if json.Unmarshal(output.Bytes(), &result) != nil || json.Unmarshal(output.Bytes(), &fields) != nil || len(fields) != 5 ||
			result.Version != 1 || result.AuditOrigin != s.AuditOrigin || !bytes.Equal(result.SourcePublic, s.SourcePublic) ||
			len(result.RecipientPublic) != 32 || result.CreatedAt.IsZero() || result.CreatedAt.Location() != time.UTC {
			t.Fatal("command output contains secrets or incorrect public registration metadata")
		}
		if reference == nil {
			reference = bytes.Clone(output.Bytes())
		} else if !bytes.Equal(reference, output.Bytes()) {
			t.Fatal("exact retry changed public registration")
		}
		assertCounts(1, 1)
	}
	bad := s
	bad.SourcePublic = bytes.Repeat([]byte{4}, 32)
	raw, err := json.Marshal(bad)
	if err != nil || os.WriteFile(path, raw, 0600) != nil || runGatewayCommand(args, io.Discard) != control.ErrGatewayAuditRegistration {
		t.Fatal("registered source key replaced")
	}
	assertCounts(1, 1)
	raw, err = json.Marshal(s)
	if err != nil || os.WriteFile(path, raw, 0600) != nil {
		t.Fatal("metadata reset")
	}
	if _, err := pool.Exec(context.Background(), "update gateway_audit_origins set closed_at=clock_timestamp()"); err != nil || runGatewayCommand(args, io.Discard) != control.ErrGatewayAuditRegistration {
		t.Fatal("closed registration revived")
	}
	assertCounts(1, 1)
	// Each negative uses a fresh, otherwise valid registration so an already
	// closed source cannot mask a missing readiness/schema/audit check.
	fresh := s
	fresh.AuditOrigin.OriginID, fresh.AuditOrigin.RuntimeID = uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	raw, err = json.Marshal(fresh)
	if err != nil || os.WriteFile(path, raw, 0600) != nil {
		t.Fatal("fresh registration metadata")
	}
	var originalHash []byte
	if err := pool.QueryRow(context.Background(), "select event_hash from audit_events").Scan(&originalHash); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), "update audit_events set event_hash=decode(repeat('00',32),'hex')"); err != nil || runGatewayCommand(args, io.Discard) != control.ErrGatewayAuditRegistration {
		t.Fatal("invalid audit chain allowed registration")
	}
	assertCounts(1, 1)
	if _, err := pool.Exec(context.Background(), "update audit_events set event_hash=$1", originalHash); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), "update goose_db_version set is_applied=false where version_id=15"); err != nil || runGatewayCommand(args, io.Discard) != control.ErrGatewayAuditRegistration {
		t.Fatal("incompatible schema allowed registration")
	}
	assertCounts(1, 1)
	if _, err := pool.Exec(context.Background(), "update goose_db_version set is_applied=true where version_id=15"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), "alter table gateway_audit_origins rename to unavailable_origins"); err != nil {
		t.Fatal(err)
	}
	var rejected bytes.Buffer
	if runGatewayCommand(args, &rejected) != control.ErrGatewayAuditRegistration || rejected.Len() != 0 {
		t.Fatal("registration DB failure exposed an error or public success output")
	}
}

func TestGatewayAuditCommandProductionRootRejected(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("production root rejection runs in the dedicated Actions step")
	}
	pool, path, _ := auditCommandFixture(t)
	t.Setenv("RESTFLEET_ENV", "production")
	config, err := control.LoadRuntimeConfig()
	clear(config.MasterKey)
	clear(config.GatewaySigningKey)
	clear(config.GatewayPendingDecryptionKey)
	if err != nil {
		t.Fatal("root fixture did not load protected production config")
	}
	var output bytes.Buffer
	if runGatewayCommand([]string{"gateway-audit-register", "--config-file", path}, &output) != control.ErrGatewayAuditRegistration || output.Len() != 0 {
		t.Fatal("production root command accepted")
	}
	var origins int
	if err := pool.QueryRow(context.Background(), "select count(*) from gateway_audit_origins").Scan(&origins); err != nil || origins != 0 {
		t.Fatal("production root command touched registration store")
	}
}
