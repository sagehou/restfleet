package httpapi

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestLostReconciliationClosesOldBootOperations verifies that a new Agent boot
// gives the Agent a grace window to replay a recovered result, then closes the
// operations that belonged to the dead boot without touching the new one.
func TestLostReconciliationClosesOldBootOperations(t *testing.T) {
	store, pool, _, enrolled, repository, _ := deliveryFixture(t, "onedrive")
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	var userID, installID uuid.UUID
	if err := pool.QueryRow(ctx, "select id from users limit 1").Scan(&userID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, "select install_id from agents where id=$1", enrolled.AgentId).Scan(&installID); err != nil {
		t.Fatal(err)
	}
	templateID, planID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	hash := strings.Repeat("d", 64)
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
		values($1,'lost-reconcile',$2,$3,$4,$5,1,'{}'::jsonb,$6,'ACTIVE',1,0,$7,$7)`,
		planID, repository.HostId, enrolled.AgentId, repository.Id, templateID, hash, now); err != nil {
		t.Fatal(err)
	}
	connect := func(bootID string, at time.Time) {
		t.Helper()
		if _, err := store.MarkAgentConnected(ctx, enrolled.AgentId, installID,
			"0.2.0", "1.0", "lost-host", bootID, "", 0, at); err != nil {
			t.Fatal(err)
		}
	}
	insertRunning := func(startedAt time.Time) uuid.UUID {
		t.Helper()
		id := uuid.Must(uuid.NewV7())
		if _, err := pool.Exec(ctx, `insert into backup_operations(id,plan_id,agent_id,type,source,status,
			plan_revision,config_hash,scheduled_at,started_at,created_at,updated_at)
			values($1,$2,$3,'BACKUP','SCHEDULED','RUNNING',1,$4,$5,$5,$5,$5)`,
			id, planID, enrolled.AgentId, hash, startedAt); err != nil {
			t.Fatal(err)
		}
		return id
	}
	status := func(id uuid.UUID) (string, string) {
		t.Helper()
		var value, code string
		if err := pool.QueryRow(ctx, "select status,error_code from backup_operations where id=$1", id).Scan(&value, &code); err != nil {
			t.Fatal(err)
		}
		return value, code
	}

	connect("boot-a", now.Add(-time.Hour))
	oldID := insertRunning(now.Add(-50 * time.Minute))
	connect("boot-b", now)
	currentID := insertRunning(now.Add(time.Second))

	// The Agent restart is still inside its replay window.
	if lost, err := store.ReconcileLostOperations(ctx, now.Add(-time.Minute), now.Add(time.Minute)); err != nil || lost != 0 {
		t.Fatalf("reconciliation fired inside grace: %d, %v", lost, err)
	}
	if value, _ := status(oldID); value != "RUNNING" {
		t.Fatalf("old-boot operation changed to %s inside grace", value)
	}

	if lost, err := store.ReconcileLostOperations(ctx, now.Add(2*time.Minute), now.Add(3*time.Minute)); err != nil || lost != 1 {
		t.Fatalf("reconciliation = %d, %v; want 1", lost, err)
	}
	if value, code := status(oldID); value != "LOST" || code != "LOCAL_OPERATION_RECOVERED" {
		t.Fatalf("old-boot operation = %s/%s, want LOST/LOCAL_OPERATION_RECOVERED", value, code)
	}
	if value, _ := status(currentID); value != "RUNNING" {
		t.Fatalf("new-boot operation changed to %s", value)
	}
}
