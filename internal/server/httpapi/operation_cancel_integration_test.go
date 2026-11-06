package httpapi

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sagehou/restfleet/internal/domain"
)

// TestCancelDispatchRacePreventsExecution exercises the two orderings of the
// dispatch claim and a cancel. A cancel that commits first must make the claim
// fail; a cancel after the claim must stay non-terminal and be closed only
// after the Agent had a chance to prove it started.
func TestCancelDispatchRacePreventsExecution(t *testing.T) {
	store, pool, _, enrolled, repository, _ := deliveryFixture(t, "onedrive")
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	audit := func(action string, id uuid.UUID) domain.AuditEvent {
		return domain.AuditEvent{ID: uuid.Must(uuid.NewV7()), OccurredAt: now,
			ActorType: domain.ActorSystem, Action: action, ResourceType: "BACKUP_OPERATION",
			ResourceID: id, RequestID: uuid.Must(uuid.NewV7()), Result: domain.AuditSuccess}
	}
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
	canceled, err := store.CancelBackupOperation(ctx, neverDispatched, "stop before start", now, testBackupRequest(neverDispatched.String()), audit("BACKUP_OPERATION_CANCEL", neverDispatched))
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
	canceled, err = store.CancelBackupOperation(ctx, claimedID, "stop in flight", now.Add(3*time.Second), testBackupRequest(claimedID.String()), audit("BACKUP_OPERATION_CANCEL", claimedID))
	if err != nil {
		t.Fatal(err)
	}
	if canceled.Status != "CANCEL_REQUESTED" {
		t.Fatalf("cancel after claim = %s, want CANCEL_REQUESTED", canceled.Status)
	}
	// A late JobAccepted must not turn a canceled row back into RUNNING, but it
	// proves the Agent received the job, so the unstarted closure must skip it.
	if err := store.MarkOperationDispatched(ctx, enrolled.AgentId, claimedID); err != nil {
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
	if _, err := store.CancelBackupOperation(ctx, undeliveredID, "stop undelivered", now.Add(5*time.Second), testBackupRequest(undeliveredID.String()), audit("BACKUP_OPERATION_CANCEL", undeliveredID)); err != nil {
		t.Fatal(err)
	}
	if closed, err := store.CloseUnstartedCancellations(ctx, now.Add(time.Minute), now.Add(2*time.Minute)); err != nil || closed != 1 {
		t.Fatalf("closure = %d, %v; want 1", closed, err)
	}
	if got := status(undeliveredID); got != "CANCELED" {
		t.Fatalf("unstarted cancellation closed as %s", got)
	}

	// An unavailable audit writer must leave both queue admission and cancel
	// untouched. Otherwise the caller sees failure while Restic still starts.
	if _, err := pool.Exec(ctx, `create function reject_manual_backup_audit() returns trigger language plpgsql as $$ begin
		if new.action in ('PLAN_BACKUP_TRIGGER','BACKUP_OPERATION_RETRY','BACKUP_OPERATION_CANCEL') then raise exception 'audit unavailable'; end if;
		return new; end $$;
		create trigger reject_manual_backup before insert on audit_events for each row execute function reject_manual_backup_audit();`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "drop trigger if exists reject_manual_backup on audit_events; drop function if exists reject_manual_backup_audit()")
	})
	protected := insertOperation()
	if _, err := store.CancelBackupOperation(ctx, protected, "audit must succeed", now, testBackupRequest(protected.String()), audit("BACKUP_OPERATION_CANCEL", protected)); err == nil {
		t.Fatal("cancel succeeded without audit")
	}
	if got := status(protected); got != "QUEUED" {
		t.Fatalf("audit failure committed cancel: %s", got)
	}
	for _, action := range []string{"PLAN_BACKUP_TRIGGER", "BACKUP_OPERATION_RETRY"} {
		id := uuid.Must(uuid.NewV7())
		operation := domain.BackupOperation{ID: id, PlanID: planID, AgentID: enrolled.AgentId,
			PlanRevision: 1, ConfigHash: hash, ScheduledAt: &now, CreatedAt: now}
		if _,err := store.CreateManualOperation(ctx, operation, testBackupRequest(id.String()), audit(action, id)); err == nil {
			t.Fatal("manual backup succeeded without audit")
		}
		var count int
		if err := pool.QueryRow(ctx, "select count(*) from backup_operations where id=$1", id).Scan(&count); err != nil || count != 0 {
			t.Fatalf("audit failure left dispatchable operation: count=%d err=%v", count, err)
		}
	}
}
