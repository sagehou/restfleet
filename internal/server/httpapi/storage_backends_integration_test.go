package httpapi

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/sagehou/restfleet/internal/rclone"
)

func backendFixture(backend string) string {
	raw := credentialFixture()
	switch backend {
	case "drive":
		raw = strings.Replace(raw, "type = onedrive", "type = drive", 1)
		return strings.Replace(raw, "drive_id = example-drive\ndrive_type = personal", "client_id = test-client.apps.googleusercontent.com\nclient_secret = google-secret-canary\nscope = drive\nroot_folder_id = fixed-root", 1)
	case "webdav":
		_, crypt, _ := strings.Cut(raw, "[encrypted]")
		return "[cloud]\ntype = webdav\nurl = https://dav.example.test/files/\nbearer_token = webdav-bearer-canary\n[encrypted]" + crypt
	default:
		return raw
	}
}

func createBackendCredential(t *testing.T, b *testBrowser, backend string) StorageCredential {
	t.Helper()
	response := b.request(t, http.MethodPost, "/api/v1/storage-credentials", StorageCredentialCreate{
		Name: backend, RemoteName: "encrypted", RcloneConfig: credentialConfig(backendFixture(backend)),
	}, map[string]string{"X-CSRF-Token": b.cookies[csrfCookieName].Value})
	if response.Code != http.StatusCreated {
		t.Fatalf("backend %s rejected: %d", backend, response.Code)
	}
	var c StorageCredential
	decodeResponse(t, response, &c)
	return c
}

func TestStorageBackendMetadataIsolationAndReplacement(t *testing.T) {
	store, _, _, b, _ := setupFleet(t)
	for _, tc := range []struct{ backend, provider string }{{"onedrive", "RCLONE_ONEDRIVE"}, {"drive", "RCLONE_GDRIVE"}, {"webdav", "RCLONE_WEBDAV"}} {
		c := createBackendCredential(t, b, tc.backend)
		if string(c.Provider) != tc.provider || c.Status != "UNTESTED" {
			t.Fatal("provider not inferred from config")
		}
		endpoint := "/api/v1/storage-credentials/" + c.Id.String()
		for _, path := range []string{"/api/v1/storage-credentials", endpoint} {
			response := b.request(t, http.MethodGet, path, nil, nil)
			assertNoStorageSecret(t, response.Body.String())
			for _, secret := range []string{"google-secret-canary", "webdav-bearer-canary", "dav.example.test", "fixed-root"} {
				if strings.Contains(response.Body.String(), secret) {
					t.Fatal("backend detail leaked")
				}
			}
		}
		host := createTestHost(t, b, "Host "+tc.backend)
		repo := createTestRepository(t, b, host.Id, c.Id, "Repo "+tc.backend)
		if repo.Status != "PROVISIONING" {
			t.Fatal("new backend claimed ready")
		}
		raw := backendFixture(tc.backend)
		headers := map[string]string{"X-CSRF-Token": b.cookies[csrfCookieName].Value, "If-Match": "\"1\""}
		changed := strings.Replace(raw, "cloud:backups", "cloud:other", 1)
		if b.request(t, http.MethodPost, endpoint+"/replace-secret", StorageCredentialReplace{RcloneConfig: &changed}, headers).Code != http.StatusConflict {
			t.Fatal("backend target moved")
		}
		next := strings.ReplaceAll(raw, "storage-refresh-canary", "new-storage-token")
		if tc.backend == "webdav" {
			next = strings.Replace(raw, "webdav-bearer-canary", "next-webdav-token", 1)
		}
		response := b.request(t, http.MethodPost, endpoint+"/replace-secret", StorageCredentialReplace{RcloneConfig: &next}, headers)
		if response.Code != http.StatusOK {
			t.Fatal("same-target secret replacement failed")
		}
		decodeResponse(t, response, &c)
		if c.SecretRevision != 2 || string(c.Provider) != tc.provider {
			t.Fatal("replacement changed provider")
		}
	}
	if err := store.VerifyAuditChain(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Provider is not a caller-controlled field, even when it names a valid backend.
	spoofed := map[string]any{"name": "spoof", "remote_name": "encrypted", "rclone_config": backendFixture("drive"), "provider": "RCLONE_ONEDRIVE"}
	if b.request(t, http.MethodPost, "/api/v1/storage-credentials", spoofed, map[string]string{"X-CSRF-Token": b.cookies[csrfCookieName].Value}).Code != http.StatusBadRequest {
		t.Fatal("caller controlled provider")
	}
}

func TestGoogleRefreshAndStaticWebDAVJobs(t *testing.T) {
	for _, backend := range []string{"drive", "webdav"} {
		t.Run(backend, func(t *testing.T) {
			store, _, _, b, _ := setupFleet(t)
			c := createBackendCredential(t, b, backend)
			o := queueCredentialTest(t, b, c.Id, "initial-test")
			worker := credentialWorker(t, store, func(ctx context.Context, raw []byte, remote string, persist func(context.Context, []byte) error) error {
				config, err := rclone.ParseConfig(string(raw), remote)
				if err != nil || config.Backend() != backend {
					t.Fatal("worker loaded wrong backend")
				}
				if backend == "drive" {
					next := bytes.ReplaceAll(raw, []byte("storage-refresh-canary"), []byte("google-refreshed-token"))
					defer clear(next)
					return persist(ctx, next)
				}
				return nil
			})
			if worked, err := worker.ProcessCredentialJob(context.Background(), uuid.Must(uuid.NewV7())); err != nil || !worked {
				t.Fatal("job not consumed")
			}
			op, err := store.Operation(context.Background(), o.Id)
			if err != nil || op.Status != "SUCCEEDED" {
				t.Fatal("backend test job failed")
			}
			current, err := store.StorageCredential(context.Background(), c.Id)
			if err != nil {
				t.Fatal(err)
			}
			if backend == "drive" && (current.SecretRevision != 2 || current.LastRefreshedAt == nil) {
				t.Fatal("Google refresh not durable")
			}
			if backend == "webdav" && (current.SecretRevision != 1 || current.LastRefreshedAt != nil) {
				t.Fatal("static credentials marked refreshed")
			}
			o = queueCredentialTest(t, b, c.Id, "unsafe-change")
			worker = credentialWorker(t, store, func(ctx context.Context, raw []byte, _ string, persist func(context.Context, []byte) error) error {
				next := bytes.ReplaceAll(raw, []byte("google-secret-canary"), []byte("changed-client"))
				if backend == "webdav" {
					next = bytes.ReplaceAll(raw, []byte("webdav-bearer-canary"), []byte("changed-bearer"))
				}
				defer clear(next)
				err := persist(ctx, next)
				if !errors.Is(err, rclone.ErrConfigChanged) {
					t.Fatal("worker accepted non-token mutation")
				}
				return err
			})
			if _, err := worker.ProcessCredentialJob(context.Background(), uuid.Must(uuid.NewV7())); err != nil {
				t.Fatal(err)
			}
			op, err = store.Operation(context.Background(), o.Id)
			if err != nil || op.Status != "FAILED" || op.ErrorCode != "CONFIG_UNSAFE" {
				t.Fatal("unsafe backend change not failed closed")
			}
			after, err := store.StorageCredential(context.Background(), c.Id)
			if err != nil || after.SecretRef != current.SecretRef {
				t.Fatal("failed refresh changed ciphertext reference")
			}
			if err := store.VerifyAuditChain(context.Background()); err != nil {
				t.Fatal(err)
			}
		})
	}
}
