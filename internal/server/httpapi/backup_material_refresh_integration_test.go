package httpapi

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sagehou/restfleet/internal/domain"
	"github.com/sagehou/restfleet/internal/rclone"
)

func TestBackupMaterialConcurrentRefreshUsesCAS(t *testing.T) {
	store, pool, b, enrolled, repo, c := deliveryFixture(t, "onedrive")
	initializeDeliveryRepository(t, store, b, repo)
	request := acceptedAdmissionRequest(t, pool, c, enrolled.AgentId)
	ctx := context.Background()
	if _, err := store.ReserveBackupAdmission(ctx, request); err != nil {
		t.Fatal(err)
	}
	ready := make(chan struct{}, 2)
	proceed := make(chan struct{}, 2)
	defer close(proceed)
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for range 2 {
		wg.Go(func() {
			results <- c.WithBackupMaterial(ctx, request.ID, request.Owner, func(workCtx context.Context, _ domain.BackupAdmission, raw []byte, _ string, persist func(context.Context, []byte) error) error {
				next := bytes.Replace(raw, []byte("storage-access-canary"), []byte("concurrent-refresh-canary"), 1)
				defer clear(next)
				ready <- struct{}{}
				<-proceed
				return persist(workCtx, next)
			})
		})
	}
	for range 2 {
		select {
		case <-ready:
		case err := <-results:
			t.Fatalf("material load failed before CAS race: %v", err)
		case <-time.After(15 * time.Second):
			t.Fatal("material load hung")
		}
	}
	proceed <- struct{}{}
	proceed <- struct{}{}
	wg.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
		} else if !errors.Is(err, domain.ErrBackupAdmission) {
			t.Fatal("raw refresh error escaped")
		}
	}
	if success != 1 {
		t.Fatal("concurrent refresh did not have exactly one winner")
	}
	var count int
	if err := pool.QueryRow(ctx, "select count(*) from storage_credential_revisions where credential_id=$1", repo.StorageCredentialId).Scan(&count); err != nil || count != 2 {
		t.Fatal("CAS race left extra revisions")
	}
}

func TestBackupMaterialRefreshAuditAndExpiryRollback(t *testing.T) {
	for _, mode := range []string{"audit", "deadline", "disabled", "released"} {
		t.Run(mode, func(t *testing.T) {
			store, pool, b, enrolled, repo, c := deliveryFixture(t, "onedrive")
			initializeDeliveryRepository(t, store, b, repo)
			request := acceptedAdmissionRequest(t, pool, c, enrolled.AgentId)
			ctx := context.Background()
			if _, err := store.ReserveBackupAdmission(ctx, request); err != nil {
				t.Fatal(err)
			}
			if mode == "audit" || mode == "deadline" {
				sql := `raise exception 'refresh audit unavailable';`
				if mode == "deadline" {
					sql = `update gateway_backup_admissions set created_at=clock_timestamp()-interval '2 hours',expires_at=clock_timestamp()-interval '1 hour' where id=new.request_id;`
				}
				_, err := pool.Exec(ctx, `create function reject_material_refresh() returns trigger language plpgsql as $$ begin
					if new.action='GATEWAY_STORAGE_REFRESH' then `+sql+` end if; return new; end $$;
					create trigger reject_material_refresh before insert on audit_events for each row execute function reject_material_refresh();`)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					_, _ = pool.Exec(context.Background(), "drop trigger if exists reject_material_refresh on audit_events;drop function if exists reject_material_refresh()")
				})
			}
			err := c.WithBackupMaterial(ctx, request.ID, request.Owner, func(workCtx context.Context, _ domain.BackupAdmission, raw []byte, _ string, persist func(context.Context, []byte) error) error {
				if mode == "disabled" {
					if _, err := pool.Exec(ctx, "update storage_credentials set status='DISABLED' where id=$1", repo.StorageCredentialId); err != nil {
						return err
					}
				}
				if mode == "released" {
					// Test-only central release with no process/session ever started.
					if err := store.ReleaseBackupAdmission(ctx, request.ID, request.Owner); err != nil {
						return err
					}
				}
				next := bytes.Replace(raw, []byte("storage-access-canary"), []byte("failed-refresh-canary"), 1)
				defer clear(next)
				if !errors.Is(persist(workCtx, next), rclone.ErrRefreshPersist) {
					t.Fatal("invalid refresh committed")
				}
				return errors.New("provider-secret-canary")
			})
			if !errors.Is(err, domain.ErrBackupAdmission) || strings.Contains(err.Error(), "canary") {
				t.Fatal("runner error not redacted")
			}
			var revision int64
			var versions int
			if err = pool.QueryRow(ctx, "select secret_revision from storage_credentials where id=$1", repo.StorageCredentialId).Scan(&revision); err != nil || revision != 1 {
				t.Fatal("failed refresh changed metadata")
			}
			if err = pool.QueryRow(ctx, "select count(*) from storage_credential_revisions where credential_id=$1", repo.StorageCredentialId).Scan(&versions); err != nil || versions != 1 {
				t.Fatal("failed refresh left a version")
			}
			var orphans int
			if err = pool.QueryRow(ctx, "select count(*) from secrets s where kind='RCLONE_CONFIG' and not exists(select 1 from storage_credential_revisions r where r.secret_ref=s.id)").Scan(&orphans); err != nil || orphans != 0 {
				t.Fatal("failed refresh left orphan ciphertext")
			}
		})
	}
}
