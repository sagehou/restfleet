package httpapi

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sagehou/restfleet/internal/domain"
	"github.com/sagehou/restfleet/internal/restic"
	control "github.com/sagehou/restfleet/internal/server"
)

func TestSnapshotListCompletesForInitializedRepository(t *testing.T) {
	store, pool, _, b, _ := setupFleet(t)
	credential := createCredential(t, b, "Snapshot scan")
	host := createTestHost(t, b, "Snapshot host")
	repository := createTestRepository(t, b, host.Id, credential.Id, "Snapshot repo")
	ctx := context.Background()

	if _, err := pool.Exec(ctx, `update repositories
		set status='READY',initialized_at=clock_timestamp(),restic_id=$2,format_version=2 where id=$1`,
		repository.Id, strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	var userID uuid.UUID
	if err := pool.QueryRow(ctx, "select id from users limit 1").Scan(&userID); err != nil {
		t.Fatal(err)
	}
	snapshotID, treeID := strings.Repeat("b", 64), strings.Repeat("c", 64)
	operation, err := store.EnqueueSnapshotList(ctx, domain.Operation{
		ID: uuid.Must(uuid.NewV7()), RepositoryID: &repository.Id, RequestedByUserID: userID,
	}, snapshotID, "/")
	if err != nil {
		t.Fatal(err)
	}

	worker, err := control.NewControlPlane(store, control.Settings{
		MasterKey: bytes.Repeat([]byte{8}, 32),
		ListSnapshot: func(_ context.Context, request restic.ListRequest, _ func(context.Context, []byte) error) (restic.ListResult, error) {
			if request.RepositoryID != repository.Id || request.SnapshotID != snapshotID || request.Path != "/" {
				return restic.ListResult{}, restic.ErrInvalidListRequest
			}
			return restic.ListResult{
				TreeID: treeID, Scanned: 1,
				Entries: []restic.Node{{Name: "file.txt", Path: "/file.txt", Type: "file", Size: 4, ModTime: time.Now().UTC()}},
			}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	worked, err := worker.ProcessCredentialJob(ctx, uuid.Must(uuid.NewV7()))
	if err != nil || !worked {
		t.Fatalf("snapshot worker = %v, %v", worked, err)
	}

	var status string
	if err := pool.QueryRow(ctx, "select status from operations where id=$1", operation.ID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "SUCCEEDED" {
		t.Fatalf("snapshot operation status = %s", status)
	}
	var cached int
	if err := pool.QueryRow(ctx, "select count(*) from snapshot_list_results where operation_id=$1", operation.ID).Scan(&cached); err != nil {
		t.Fatal(err)
	}
	if cached != 1 {
		t.Fatalf("cached snapshot pages = %d", cached)
	}
}
