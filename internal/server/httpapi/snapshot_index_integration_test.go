package httpapi

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sagehou/restfleet/internal/domain"
)

func TestSnapshotIndexMarksManagedAndMissing(t *testing.T) {
	store, pool, _, enrolled, repository, _ := deliveryFixture(t, "onedrive")
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	if _, err := pool.Exec(ctx, `update repositories
		set status='READY',initialized_at=$2,restic_id=$3,format_version=2 where id=$1`,
		repository.Id, now, strings.Repeat("f", 64)); err != nil {
		t.Fatal(err)
	}
	var userID uuid.UUID
	if err := pool.QueryRow(ctx, "select id from users limit 1").Scan(&userID); err != nil {
		t.Fatal(err)
	}
	templateID, planID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	hash := strings.Repeat("e", 64)
	if _, err := pool.Exec(ctx, `insert into templates(id,name,current_revision,created_at,updated_at)
		values($1,$2,1,$3,$3)`, templateID, templateID.String(), now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `insert into template_revisions(template_id,revision,config_json,config_hash,created_by,created_at)
		values($1,1,'{}'::jsonb,$2,$3,$4)`, templateID, hash, userID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `insert into plans(id,name,host_id,agent_id,repository_id,template_id,template_revision,
		effective_config,config_hash,status,desired_revision,accepted_revision,created_at,updated_at)
		values($1,$2,$3,$4,$5,$6,1,'{}'::jsonb,$7,'ACTIVE',1,0,$8,$8)`,
		planID, planID.String(), repository.HostId, enrolled.AgentId, repository.Id, templateID, hash, now); err != nil {
		t.Fatal(err)
	}

	managedID := strings.Repeat("1", 64)
	unmanagedID := strings.Repeat("2", 64)
	entry := func(id string, plan *uuid.UUID) domain.IndexedSnapshot {
		return domain.IndexedSnapshot{
			SnapshotID: id, TreeID: strings.Repeat("a", 64), CapturedAt: now,
			PlanID: plan, Tags: []string{"restfleet:managed"}, Paths: []string{"/srv"},
			ProgramVersion: "restic 0.19.1", FilesNew: 2, BytesAdded: 1024,
		}
	}
	index := func(entries []domain.IndexedSnapshot) {
		t.Helper()
		operationID := uuid.Must(uuid.NewV7())
		operation, err := store.EnqueueSnapshotIndex(ctx, domain.Operation{ID: operationID, RepositoryID: &repository.Id})
		if err != nil {
			t.Fatal(err)
		}
		owner := uuid.Must(uuid.NewV7())
		job, err := store.ClaimCredentialJob(ctx, owner)
		if err != nil || job.Operation.ID != operation.ID {
			t.Fatalf("claim = %+v, %v", job, err)
		}
		if err := store.FinishSnapshotIndexJob(ctx, job.ID, owner, "", domain.SnapshotIndexResult{Entries: entries}); err != nil {
			t.Fatal(err)
		}
	}

	index([]domain.IndexedSnapshot{entry(managedID, &planID), entry(unmanagedID, nil)})
	var managed bool
	var managedPlan *uuid.UUID
	if err := pool.QueryRow(ctx, `select managed,plan_id from snapshots where repository_id=$1 and snapshot_id=$2`,
		repository.Id, managedID).Scan(&managed, &managedPlan); err != nil {
		t.Fatal(err)
	}
	if !managed || managedPlan == nil || *managedPlan != planID {
		t.Fatalf("managed snapshot = %v/%v", managed, managedPlan)
	}
	var unmanagedPlan *uuid.UUID
	if err := pool.QueryRow(ctx, `select managed,plan_id from snapshots where repository_id=$1 and snapshot_id=$2`,
		repository.Id, unmanagedID).Scan(&managed, &unmanagedPlan); err != nil {
		t.Fatal(err)
	}
	if managed || unmanagedPlan != nil {
		t.Fatalf("unmanaged snapshot = %v/%v", managed, unmanagedPlan)
	}

	index([]domain.IndexedSnapshot{entry(managedID, &planID)})
	var missing *time.Time
	if err := pool.QueryRow(ctx, `select missing_at from snapshots where repository_id=$1 and snapshot_id=$2`,
		repository.Id, unmanagedID).Scan(&missing); err != nil {
		t.Fatal(err)
	}
	if missing == nil {
		t.Fatal("missing snapshot was not marked after a complete index")
	}
}
