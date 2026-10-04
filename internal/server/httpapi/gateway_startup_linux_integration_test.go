package httpapi

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sagehou/restfleet/internal/gateway"
	"github.com/sagehou/restfleet/internal/gatewaypending"
	"github.com/sagehou/restfleet/internal/persistence/postgres"
	"github.com/sagehou/restfleet/internal/rclone"
	"github.com/sagehou/restfleet/internal/security"
	control "github.com/sagehou/restfleet/internal/server"
	"golang.org/x/crypto/nacl/box"
)

// Deliberately starts without any grant, source registration or material access.
// The coordinator must perform all three against the real PostgreSQL store.
func startupIntegrationFixture(t *testing.T, backend string) (pendingIntegration, control.GatewayStartupConfig) {
	t.Helper()
	s, pool, b, enrolled, repo, previous := deliveryFixture(t, backend)
	initializeDeliveryRepository(t, s, b, repo)
	r := acceptedAdmissionRequest(t, pool, previous, enrolled.AgentId)
	a, err := s.ReserveBackupAdmission(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	central, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(key)
	recipient, private, err := box.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(private[:])
	c, err := control.NewControlPlane(s, control.Settings{MasterKey: bytes.Repeat([]byte{8}, 32), GatewayPublicURL: "https://gateway.example",
		GatewaySigningKey: key, GatewayPendingDecryptionKey: private[:], Enrollment: control.EnrollmentSettings{ServerCABundlePEM: []byte(enrolled.CaBundlePem)},
		ExpectedSchema: postgres.ExpectedSchemaVersion, PasswordParams: security.Argon2Params{Memory: 64, Iterations: 1, Parallelism: 1, SaltLength: 8, KeyLength: 16}})
	if err != nil {
		t.Fatal(err)
	}
	sourcePublic, source, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { clear(source) })
	binding := security.GatewayAuthorizationBinding{AdmissionID: a.ID, Owner: a.Owner, RuntimeID: uuid.Must(uuid.NewV7()), AgentID: a.AgentID,
		HostID: a.HostID, RepositoryID: a.RepositoryID, GatewayID: a.GatewayID, StorageCredentialID: a.StorageCredentialID, DeliveryID: a.DeliveryID,
		GatewaySecretRef: a.GatewaySecretRef, ResticSecretRef: a.ResticSecretRef, ConfigurationHash: a.ConfigurationHash}
	startup := control.GatewayStartupConfig{Version: 1, Binding: binding, SourcePublic: sourcePublic, DecisionID: uuid.Must(uuid.NewV7()),
		LifetimeSeconds: 600, GatewayUID: uint32(os.Geteuid())}
	f := pendingIntegration{store: s, pool: pool, control: c, statement: security.GatewayStatement{Binding: binding},
		centralPublic: central, sourcePublic: sourcePublic, source: source, recipient: *recipient, browser: b}
	return f, startup
}

func startupExchange(t *testing.T, f pendingIntegration, startup control.GatewayStartupConfig,
	install func(context.Context, security.GatewayMaterial) (func(), error),
) (error, error) {
	t.Helper()
	receiver, err := gatewaypending.NewMaterialReceiver(f.statement.Binding, f.source, f.centralPublic)
	if err != nil {
		t.Fatal(err)
	}
	defer receiver.Close()
	dir, err := os.MkdirTemp("", "rfg-startup-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	startup.SocketPath = filepath.Join(dir, "material.sock")
	l, err := gatewaypending.ListenReplay(startup.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	metadataDir := t.TempDir()
	if os.Chmod(metadataDir, 0700) != nil {
		t.Fatal("private metadata directory")
	}
	metadata := filepath.Join(metadataDir, "startup.json")
	raw, err := json.Marshal(startup)
	if err != nil || os.WriteFile(metadata, raw, 0600) != nil {
		t.Fatal("protected startup fixture")
	}
	loaded, loadErr := control.LoadGatewayStartupConfig(metadata)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- receiver.Receive(ctx, l, uint32(os.Geteuid()), install, f.control.RecordGatewayMaterialDenied)
	}()
	if loadErr == nil {
		err = f.control.InitializeGateway(ctx, loaded)
	} else {
		err = loadErr
	}
	if err != nil {
		cancel()
	}
	return err, <-done
}

func startupCounts(t *testing.T, f pendingIntegration, decisions, origins, intents int) {
	t.Helper()
	var d, o, i, accesses int
	err := f.pool.QueryRow(context.Background(), `select (select count(*) from gateway_authorization_decisions),
		(select count(*) from gateway_pending_origins),(select count(*) from gateway_material_deliveries),
		(select count(*) from audit_events where action='GATEWAY_MATERIAL_DELIVERY' and reason_code='SECRET_ACCESS')`).Scan(&d, &o, &i, &accesses)
	if err != nil || d != decisions || o != origins || i != intents || accesses != intents {
		t.Fatalf("startup effects: decision=%d origin=%d intent=%d access=%d: %v", d, o, i, accesses, err)
	}
	if (decisions != 0 || origins != 0) && f.store.ReleaseBackupAdmission(context.Background(), f.statement.Binding.AdmissionID, f.statement.Binding.Owner) == nil {
		t.Fatal("initialization/uncertain failure released occupied fence")
	}
}

func TestGatewayStartupCoordinatesFirstGrantSourceAndMaterial(t *testing.T) {
	for _, backend := range []string{"onedrive", "drive", "webdav"} {
		t.Run(backend, func(t *testing.T) {
			f, startup := startupIntegrationFixture(t, backend)
			var borrowed []byte
			install := func(_ context.Context, m security.GatewayMaterial) (func(), error) {
				borrowed = m.Config
				parsed, err := rclone.ParseConfig(string(m.Config), m.Remote)
				if err != nil || parsed.Backend() != backend || m.SecretRevision != 1 || m.PendingRecipient != f.recipient || m.Challenge.Binding != startup.Binding {
					return nil, security.ErrGatewayMaterial
				}
				a, err := gateway.NewAuthorization(f.centralPublic, startup.Binding)
				if err != nil || a.Accept(m.Statement) != nil || a.Status() != gateway.AuthorizationValid {
					return nil, security.ErrGatewayMaterial
				}
				statement, err := security.VerifyGatewayStatement(m.Statement, f.centralPublic)
				if err != nil || statement.Revision != 1 {
					return nil, security.ErrGatewayMaterial
				}
				return func() {}, nil
			}
			if err, received := startupExchange(t, f, startup, install); err != nil || received != nil || len(borrowed) == 0 || len(bytes.Trim(borrowed, "\x00")) != 0 {
				t.Fatalf("startup exchange: %v / %v", err, received)
			}
			startupCounts(t, f, 1, 1, 1)
			// Same trusted metadata against a NEW receiver/challenge cannot reissue.
			if err, received := startupExchange(t, f, startup, install); err != control.ErrGatewayStartup || received == nil {
				t.Fatal("explicit restart reissued material")
			}
			startupCounts(t, f, 1, 1, 1)
			startup.Binding.RuntimeID = uuid.Must(uuid.NewV7())
			startup.DecisionID = uuid.Must(uuid.NewV7())
			f.statement.Binding = startup.Binding
			if err, received := startupExchange(t, f, startup, install); err != control.ErrGatewayStartup || received == nil {
				t.Fatal("fresh incarnation took over an old admission")
			}
			startupCounts(t, f, 1, 1, 1)
		})
	}
}

func TestGatewayStartupRejectsUntrustedPinsAndRetainsPartialCommit(t *testing.T) {
	for _, failure := range []string{"source", "binding", "stored-binding", "uid", "configuration", "disabled", "ack", "grant-audit", "registration", "material-audit", "install"} {
		t.Run(failure, func(t *testing.T) {
			f, startup := startupIntegrationFixture(t, "onedrive")
			decisions, origins, intents := 0, 0, 0
			var err error
			switch failure {
			case "source":
				startup.SourcePublic = f.centralPublic
			case "binding":
				startup.Binding.HostID = uuid.Must(uuid.NewV7())
			case "stored-binding":
				startup.Binding.HostID = uuid.Must(uuid.NewV7())
				f.statement.Binding = startup.Binding
				decisions = 1
			case "uid":
				startup.GatewayUID++
			case "configuration":
				startup.Binding.ConfigurationHash = security.GatewayPendingHash([]byte("different-config"))
			case "disabled":
				_, err = f.pool.Exec(context.Background(), "update storage_credentials set status='DISABLED' where id=$1", startup.Binding.StorageCredentialID)
			case "ack":
				_, err = f.pool.Exec(context.Background(), "update repository_agent_deliveries set accepted_at=null where id=$1", startup.Binding.DeliveryID)
			case "grant-audit", "material-audit":
				action := "GATEWAY_AUTHORIZATION_DECISION"
				if failure == "material-audit" {
					action, decisions, origins = "GATEWAY_MATERIAL_DELIVERY", 1, 1
				}
				_, err = f.pool.Exec(context.Background(), `create function reject_startup_audit() returns trigger language plpgsql as $$ begin
				if NEW.action='`+action+`' then raise exception 'private-database-canary'; end if; return NEW; end $$;
				create trigger reject_startup_audit before insert on audit_events for each row execute function reject_startup_audit()`)
				t.Cleanup(func() {
					if _, err := f.pool.Exec(context.Background(), "drop trigger if exists reject_startup_audit on audit_events;drop function if exists reject_startup_audit()"); err != nil {
						t.Error("audit injection cleanup", err)
					}
				})
			case "registration":
				decisions = 1
				_, err = f.pool.Exec(context.Background(), `create function reject_startup_origin() returns trigger language plpgsql as $$ begin
				raise exception 'private-registration-canary'; end $$;
				create trigger reject_startup_origin before insert on gateway_pending_origins for each row execute function reject_startup_origin()`)
				t.Cleanup(func() {
					if _, err := f.pool.Exec(context.Background(), "drop trigger if exists reject_startup_origin on gateway_pending_origins;drop function if exists reject_startup_origin()"); err != nil {
						t.Error("registration injection cleanup", err)
					}
				})
			case "install":
				decisions, origins, intents = 1, 1, 1
			}
			if err != nil {
				t.Fatal(err)
			}
			installs, rollbacks := 0, 0
			install := func(_ context.Context, _ security.GatewayMaterial) (func(), error) {
				installs++
				return func() { rollbacks++ }, errors.New("private-install-canary")
			}
			if err, received := startupExchange(t, f, startup, install); err != control.ErrGatewayStartup || received == nil {
				t.Fatal("unauthorized/failed startup succeeded or exposed raw error")
			}
			if installs != intents || rollbacks != intents {
				t.Fatal("failed transaction delivered plaintext or failed installation lacked rollback")
			}
			startupCounts(t, f, decisions, origins, intents)
			if failure == "install" {
				if err, _ := startupExchange(t, f, startup, install); err != control.ErrGatewayStartup || installs != 1 {
					t.Fatal("uncertain failed installation repeated material")
				}
				startupCounts(t, f, 1, 1, 1)
			}
		})
	}
}
