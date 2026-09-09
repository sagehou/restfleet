package httpapi

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sagehou/restfleet/internal/domain"
	"github.com/sagehou/restfleet/internal/rclone"
	"github.com/sagehou/restfleet/internal/security"
)

func TestBackupMaterialProvidersAuditRefreshAndBorrowLifetime(t *testing.T) {
	for _, backend := range []string{"onedrive", "drive", "webdav"} {
		t.Run(backend, func(t *testing.T) {
			store, pool, b, enrolled, repo, c := deliveryFixture(t, backend)
			initializeDeliveryRepository(t, store, b, repo)
			request := acceptedAdmissionRequest(t, pool, c, enrolled.AgentId)
			ctx := context.Background()
			if _, err := store.ReserveBackupAdmission(ctx, request); err != nil {
				t.Fatal(err)
			}
			var borrowed, nextRaw []byte
			var retained func(context.Context, []byte) error
			err := c.WithBackupMaterial(ctx, request.ID, request.Owner, func(workCtx context.Context, a domain.BackupAdmission, raw []byte, remote string, persist func(context.Context, []byte) error) error {
				borrowed, retained = raw, persist
				if a.RepositoryID != repo.Id || a.HostID != enrolled.HostId || remote != "encrypted" {
					t.Fatal("wrong material binding")
				}
				deadline, ok := workCtx.Deadline()
				if !ok || deadline.After(a.ExpiresAt) {
					t.Fatal("unbounded material lifetime")
				}
				var count int
				if err := pool.QueryRow(ctx, "select count(*) from audit_events where action='GATEWAY_STORAGE_SECRET_ACCESS' and request_id=$1", a.ID).Scan(&count); err != nil || count != 1 {
					t.Fatal("material escaped before committed audit")
				}
				// Identical config must not manufacture a refreshed revision.
				if err := persist(workCtx, raw); err != nil {
					return err
				}
				if backend == "webdav" {
					changed := bytes.Replace(raw, []byte("webdav-bearer-canary"), []byte("different-bearer"), 1)
					defer clear(changed)
					if !errors.Is(persist(workCtx, changed), rclone.ErrConfigChanged) {
						t.Fatal("WebDAV auth changed through token refresh")
					}
					return nil
				}
				// Replace the JSON access_token value only; works for both OAuth fixtures.
				text := string(raw)
				start := strings.Index(text, "\"access_token\":\"")
				if start < 0 {
					t.Fatal("OAuth fixture missing")
				}
				start += len("\"access_token\":\"")
				end := start + strings.Index(text[start:], "\"")
				nextRaw = []byte(text[:start] + "admission-refreshed-canary" + text[end:])
				if err := persist(workCtx, nextRaw); err != nil {
					return err
				}
				changed := bytes.Replace(nextRaw, []byte("cloud:backups"), []byte("cloud:other"), 1)
				defer clear(changed)
				if !errors.Is(persist(workCtx, changed), rclone.ErrConfigChanged) {
					t.Fatal("refresh changed target")
				}
				return nil
			})
			defer clear(nextRaw)
			if err != nil {
				t.Fatal("material runner failed")
			}
			if len(bytes.Trim(borrowed, "\x00")) != 0 {
				t.Fatal("borrowed plaintext retained")
			}
			if !errors.Is(retained(ctx, nextRaw), rclone.ErrRefreshPersist) {
				t.Fatal("retained callback accepted")
			}
			m, err := store.BackupAdmissionMaterial(ctx, request.ID, request.Owner, request.ConfigurationHash)
			if err != nil {
				t.Fatal(err)
			}
			want := int64(2)
			if backend == "webdav" {
				want = 1
			}
			if m.Credential.SecretRevision != want {
				t.Fatal("incorrect secret revision")
			}
			if backend != "webdav" {
				if m.Credential.LastRefreshedAt == nil {
					t.Fatal("refresh metadata missing")
				}
				e := m.Envelope
				plain, err := security.OpenEnvelope(bytes.Repeat([]byte{8}, 32), security.Envelope{Ciphertext: e.Ciphertext, Nonce: e.Nonce, WrappedDataKey: e.WrappedDataKey, WrapNonce: e.WrapNonce, AAD: e.AAD})
				if err != nil || !bytes.Contains(plain, []byte("admission-refreshed-canary")) {
					t.Fatal("encrypted refresh not recoverable")
				}
				clear(plain)
				if bytes.Contains(e.Ciphertext, []byte("admission-refreshed-canary")) {
					t.Fatal("plaintext persisted")
				}
			}
			if _, err = store.CheckBackupAdmission(ctx, request.ID, request.Owner, request.ConfigurationHash); err != nil {
				t.Fatal("refresh changed admission")
			}
			if err = store.ReleaseBackupAdmission(ctx, request.ID, request.Owner); err != nil {
				t.Fatal(err)
			}
			called := false
			if c.WithBackupMaterial(ctx, request.ID, request.Owner, func(context.Context, domain.BackupAdmission, []byte, string, func(context.Context, []byte) error) error {
				called = true
				return nil
			}) == nil || called {
				t.Fatal("released admission obtained material")
			}
		})
	}
}

func TestBackupMaterialWrongOwnerAndStaleCAS(t *testing.T) {
	store, pool, _, repo, request := admissionFixture(t, "onedrive")
	ctx := context.Background()
	if _, err := store.ReserveBackupAdmission(ctx, request); err != nil {
		t.Fatal(err)
	}
	if m, err := store.BackupAdmissionMaterial(ctx, request.ID, uuid.Must(uuid.NewV7()), request.ConfigurationHash); err == nil || len(m.Envelope.Ciphertext) > 0 {
		t.Fatal("wrong owner obtained ciphertext")
	}
	m, err := store.BackupAdmissionMaterial(ctx, request.ID, request.Owner, request.ConfigurationHash)
	if err != nil {
		t.Fatal(err)
	}
	// These callers supply a deliberately stale expected revision; no envelope
	// or audit row may be inserted, even under duplicate concurrent delivery.
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for range 4 {
		wg.Go(func() {
			_, err := store.RefreshBackupAdmission(ctx, request.ID, request.Owner, request.ConfigurationHash, m.Credential.SecretRevision+1, m.Envelope)
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if !errors.Is(err, domain.ErrRevisionConflict) {
			t.Fatal("stale CAS was accepted")
		}
	}
	var count int
	if err = pool.QueryRow(ctx, "select count(*) from storage_credential_revisions where credential_id=$1", repo.StorageCredentialId).Scan(&count); err != nil || count != 1 {
		t.Fatal("stale refresh left a revision")
	}
}

func TestBackupMaterialAuditFailureAndDeadlineNeverReturnCiphertext(t *testing.T) {
	for _, mode := range []string{"audit", "deadline", "disabled", "expired", "hash"} {
		t.Run(mode, func(t *testing.T) {
			store, pool, _, repo, request := admissionFixture(t, "onedrive")
			ctx := context.Background()
			if _, err := store.ReserveBackupAdmission(ctx, request); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "audit", "deadline":
				sql := `raise exception 'audit unavailable';`
				if mode == "deadline" {
					sql = `update gateway_backup_admissions set created_at=clock_timestamp()-interval '2 hours',expires_at=clock_timestamp()-interval '1 hour' where id=new.request_id;`
				}
				_, err := pool.Exec(ctx, `create function reject_material_audit() returns trigger language plpgsql as $$ begin
					if new.action='GATEWAY_STORAGE_SECRET_ACCESS' then `+sql+` end if; return new; end $$;
					create trigger reject_material before insert on audit_events for each row execute function reject_material_audit();`)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					_, _ = pool.Exec(context.Background(), "drop trigger if exists reject_material on audit_events;drop function if exists reject_material_audit()")
				})
			case "disabled":
				if _, err := pool.Exec(ctx, "update storage_credentials set status='DISABLED' where id=$1", repo.StorageCredentialId); err != nil {
					t.Fatal(err)
				}
			case "expired":
				if _, err := pool.Exec(ctx, "update gateway_backup_admissions set created_at=$2,expires_at=$3 where id=$1", request.ID, time.Now().Add(-2*time.Hour), time.Now().Add(-time.Hour)); err != nil {
					t.Fatal(err)
				}
			case "hash":
				request.ConfigurationHash = strings.Repeat("b", 64)
			}
			m, err := store.BackupAdmissionMaterial(ctx, request.ID, request.Owner, request.ConfigurationHash)
			if err == nil || len(m.Envelope.Ciphertext) > 0 {
				t.Fatal("invalid material access succeeded")
			}
		})
	}
}
