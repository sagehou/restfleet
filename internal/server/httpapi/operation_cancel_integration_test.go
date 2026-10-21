package httpapi

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestCancelDispatchRacePreventsExecution exercises the two orderings of the
// dispatch claim and a cancel. A cancel that commits first must make the claim
// fail; a cancel after the claim must stay non-terminal and be closed only
// after the Agent had a chance to prove it started.
func TestCancelDispatchRacePreventsExecution(t *testing.T) {
	store, pool, _, enrolled, repository, _ := deliveryFixture(t, "onedrive")
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	var userID uuid.UUID
	if err := pool.QueryRow(ctx, "select id from users limit 1").Scan(&userID); err != nil {
		t.Fatal(err)
	}
	templateID, planID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	hash := strings.Repeat("c", 64)
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
		values($1,'cancel-race',$2,$3,$4,$5,1,'{}'::jsonb,$6,'ACTIVE',1,0,$7,$7)`,
		planID, repository.HostId, enrolled.AgentId, repository.Id, templateID, hash, now); err != nil {
		t.Fatal(err)
	}
	insertOperation := func() uuid.UUID {
		id := uuid.Must(uuid.NewV7())
		if _, err := pool.Exec(ctx, `insert into backup_operations(id,plan_id,agent_id,type,source,status,
			plan_revision,config_hash,scheduled_at,created_at,updated_at)
			values($1,$2,$3,'BACKUP','MANUAL','QUEUED',1,$4,$5,$5,$5)`,
			id, planID, enrolled.AgentId, hash, now); err != nil {
			t.Fatal(err)
		}
		return id
	}
	status := func(id uuid.UUID) string {
		var value string
		if err := pool.QueryRow(ctx, "select status from backup_operations where id=$1", id).Scan(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}

	// Cancel commits before the dispatcher claims the row: no dispatch may occur.
	neverDispatched := insertOperation()
	canceled, err := store.CancelBackupOperation(ctx, neverDispatched, "stop before start", now)
	if err != nil {
		t.Fatal(err)
	}
	if canceled.Status != "CANCELED" {
		t.Fatalf("cancel of a never-dispatched operation = %s, want CANCELED", canceled.Status)
	}
	if _, ok, err := store.ClaimManualOperationDispatch(ctx, enrolled.AgentId, now.Add(time.Second), 15*time.Second); err != nil || ok {
		t.Fatalf("canceled operation was dispatched: ok=%v err=%v", ok, err)
	}

	// Claim commits first: the cancel must remain pending and be delivered.
	claimedID := insertOperation()
	claimed, ok, err := store.ClaimManualOperationDispatch(ctx, enrolled.AgentId, now.Add(2*time.Second), 15*time.Second)
	if err != nil || !ok {
		t.Fatalf("queued operation was not claimed: %v", err)
	}
	if claimed.ID != claimedID || claimed.DispatchedAt == nil {
		t.Fatal("claim did not mark dispatched_at")
	}
	canceled, err = store.CancelBackupOperation(ctx, claimedID, "stop in flight", now.Add(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if canceled.Status != "CANCEL_REQUESTED" {
		t.Fatalf("cancel after claim = %s, want CANCEL_REQUESTED", canceled.Status)
	}
	// A late JobAccepted must not turn a canceled row back into RUNNING, but it
	// proves the Agent received the job, so the unstarted closure must skip it.
	if err := store.MarkOperationDispatched(ctx, claimedID); err != nil {
		t.Fatal(err)
	}
	if got := status(claimedID); got != "CANCEL_REQUESTED" {
		t.Fatalf("late acceptance changed status to %s", got)
	}
	var started *time.Time
	if err := pool.QueryRow(ctx, "select started_at from backup_operations where id=$1", claimedID).Scan(&started); err != nil || started == nil {
		t.Fatalf("late acceptance was not recorded as started: %v, %v", started, err)
	}
	pending, err := store.PendingCancelOperations(ctx, enrolled.AgentId)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].ID != claimedID {
		t.Fatal("pending cancellation was not delivered")
	}
	// The closure only applies after the grace window and never to a run the
	// Agent proved it received.
	if closed, err := store.CloseUnstartedCancellations(ctx, now.Add(-time.Second), now.Add(time.Minute)); err != nil || closed != 0 {
		t.Fatalf("closure fired inside the grace window: %d, %v", closed, err)
	}
	if closed, err := store.CloseUnstartedCancellations(ctx, now.Add(time.Minute), now.Add(2*time.Minute)); err != nil || closed != 0 {
		t.Fatalf("closure fired for an accepted run: %d, %v", closed, err)
	}

	// A claimed operation whose JobAccepted was lost is closed after grace.
	undeliveredID := insertOperation()
	if _, ok, err := store.ClaimManualOperationDispatch(ctx, enrolled.AgentId, now.Add(4*time.Second), 15*time.Second); err != nil || !ok {
		t.Fatalf("second queued operation was not claimed: %v", err)
	}
	if _, err := store.CancelBackupOperation(ctx, undeliveredID, "stop undelivered", now.Add(5*time.Second)); err != nil {
		t.Fatal(err)
	}
	if closed, err := store.CloseUnstartedCancellations(ctx, now.Add(time.Minute), now.Add(2*time.Minute)); err != nil || closed != 1 {
		t.Fatalf("closure = %d, %v; want 1", closed, err)
	}
	if got := status(undeliveredID); got != "CANCELED" {
		t.Fatalf("unstarted cancellation closed as %s", got)
	}
}
