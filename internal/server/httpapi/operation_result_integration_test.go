package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sagehou/restfleet/internal/domain"
)

func TestOperationTerminalGuardAndRejectedQueue(t *testing.T) {
	store, pool, b, enrolled, repository, _ := deliveryFixture(t, "onedrive")
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	var userID uuid.UUID
	if err := pool.QueryRow(ctx, "select id from users limit 1").Scan(&userID); err != nil {
		t.Fatal(err)
	}
	templateID, planID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	hash := strings.Repeat("a", 64)
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
		values($1,'operation-guard',$2,$3,$4,$5,1,'{}'::jsonb,$6,'ACTIVE',1,0,$7,$7)`,
		planID, repository.HostId, enrolled.AgentId, repository.Id, templateID, hash, now); err != nil {
		t.Fatal(err)
	}

	queued := uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(ctx, `insert into backup_operations(id,plan_id,agent_id,type,source,status,plan_revision,config_hash,
		scheduled_at,created_at,updated_at) values($1,$2,$3,'BACKUP','MANUAL','QUEUED',1,$4,$5,$5,$5)`,
		queued, planID, enrolled.AgentId, hash, now); err != nil {
		t.Fatal(err)
	}
	// A second enrolled Host may report its own plan but cannot select the ID
	// of an existing operation belonging to the first Host.
	otherHost := createTestHost(t, b, "Other operation Host")
	otherRepository := createTestRepository(t, b, otherHost.Id, repository.StorageCredentialId, "Other operation Repository")
	token := createTestEnrollmentToken(t, b, otherHost.Id)
	csr, key := agentCSR(t)
	clear(key)
	response := b.request(t, http.MethodPost, "/api/v1/agent-enrollment", enrollRequest(token.Token, csr, uuid.Must(uuid.NewV7())), nil)
	if response.Code != http.StatusCreated { t.Fatal("second Host enrollment failed") }
	var other AgentEnrollmentResponse
	decodeResponse(t, response, &other)
	otherPlan := uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(ctx, `insert into plans(id,name,host_id,agent_id,repository_id,template_id,template_revision,
		effective_config,config_hash,status,desired_revision,accepted_revision,created_at,updated_at)
		values($1,'other-operation-guard',$2,$3,$4,$5,1,'{}'::jsonb,$6,'ACTIVE',1,0,$7,$7)`,
		otherPlan, other.HostId, other.AgentId, otherRepository.Id, templateID, hash, now); err != nil { t.Fatal(err) }
	for _, attack := range []func() error{
		func() error { return store.MarkOperationDispatched(ctx, other.AgentId, queued) },
		func() error { return store.RejectManualOperation(ctx, other.AgentId, queued, now) },
		func() error { return store.UpsertOperationProgress(ctx, domain.BackupOperationProgress{
			OperationID:queued, PlanID:otherPlan, AgentID:other.AgentId, PlanRevision:1, ScheduledAt:now, ObservedAt:now, Sequence:1, Stage:"backup"}) },
		func() error { return store.UpsertOperationResult(ctx, domain.BackupOperationResult{
			OperationID:queued, PlanID:otherPlan, AgentID:other.AgentId, PlanRevision:1, ScheduledAt:now, FinishedAt:now, Status:"SUCCEEDED", SnapshotID:strings.Repeat("d",64)}) },
		func() error { return store.UpsertOperationResult(ctx, domain.BackupOperationResult{
			OperationID:queued, PlanID:planID, AgentID:enrolled.AgentId, PlanRevision:2, ScheduledAt:now, FinishedAt:now, Status:"FAILED"}) },
	} {
		if err := attack(); !errors.Is(err,domain.ErrPlanBindingInvalid) { t.Fatalf("cross-bound operation accepted: %v",err) }
	}
	protected, err := store.BackupOperation(ctx,queued)
	if err != nil || protected.Status!="QUEUED" || protected.StartedAt!=nil || protected.FinishedAt!=nil { t.Fatalf("foreign report changed operation: %+v %v",protected,err) }
	var leaked int
	if err := pool.QueryRow(ctx,"select count(*) from backup_operation_events where operation_id=$1",queued).Scan(&leaked); err!=nil || leaked!=0 {t.Fatalf("foreign event persisted: %d %v",leaked,err)}
	if err := pool.QueryRow(ctx,"select count(*) from snapshots where operation_id=$1",queued).Scan(&leaked); err!=nil || leaked!=0 {t.Fatalf("foreign snapshot persisted: %d %v",leaked,err)}
	if err := store.RejectManualOperation(ctx, enrolled.AgentId, queued, now); err != nil {
		t.Fatal(err)
	}
	var status string
	if err := pool.QueryRow(ctx, "select status from backup_operations where id=$1", queued).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "FAILED" {
		t.Fatalf("rejected queued operation status = %s, want FAILED", status)
	}

	operationID := uuid.Must(uuid.NewV7())
	result := domain.BackupOperationResult{
		OperationID: operationID, PlanID: planID, AgentID: enrolled.AgentId,
		PlanRevision: 1, ScheduledAt: now, Status: "SUCCEEDED", FinishedAt: now,
	}
	if err := store.UpsertOperationResult(ctx, result); err != nil {
		t.Fatal(err)
	}
	late := result
	late.Status, late.ErrorCode = "FAILED", "LATE_RESULT"
	if err := store.UpsertOperationResult(ctx, late); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, "select status from backup_operations where id=$1", operationID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "SUCCEEDED" {
		t.Fatalf("late result rewrote terminal status = %s", status)
	}
	var health string
	if err := pool.QueryRow(ctx, "select backup_health from plans where id=$1", planID).Scan(&health); err != nil {
		t.Fatal(err)
	}
	if health != "HEALTHY" {
		t.Fatalf("late result changed plan health = %s", health)
	}
	progressID := uuid.Must(uuid.NewV7())
	progress := domain.BackupOperationProgress{OperationID:progressID,PlanID:planID,AgentID:enrolled.AgentId,
		PlanRevision:1,ScheduledAt:now,ObservedAt:now,Sequence:2,Stage:"backup",FilesDone:8}
	if err:=store.UpsertOperationProgress(ctx,progress);err!=nil {t.Fatal(err)}
	progress.Sequence,progress.FilesDone=1,1
	if err:=store.UpsertOperationProgress(ctx,progress);err!=nil {t.Fatal(err)}
	progress.Sequence=2
	if err:=store.UpsertOperationProgress(ctx,progress);err!=nil {t.Fatal(err)}
	current,err:=store.BackupOperation(ctx,progressID)
	if err!=nil || current.FilesDone!=8 {t.Fatalf("stale/duplicate progress regressed counters: %+v %v",current,err)}
}

func TestBackupOperationPaginationAdvances(t *testing.T) {
	store, pool, _, enrolled, repository, _ := deliveryFixture(t, "onedrive")
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	var userID uuid.UUID
	if err := pool.QueryRow(ctx, "select id from users limit 1").Scan(&userID); err != nil {
		t.Fatal(err)
	}
	templateID, planID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	hash := strings.Repeat("b", 64)
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
	for range 3 {
		id := uuid.Must(uuid.NewV7())
		if _, err := pool.Exec(ctx, `insert into backup_operations(id,plan_id,agent_id,type,source,status,plan_revision,
			config_hash,scheduled_at,finished_at,created_at,updated_at)
			values($1,$2,$3,'BACKUP','SCHEDULED','FAILED',1,$4,$5,$5,$5,$5)`,
			id, planID, enrolled.AgentId, hash, now); err != nil {
			t.Fatal(err)
		}
	}
	first, err := store.BackupOperations(ctx, uuid.Nil, 2)
	if err != nil || len(first) != 2 {
		t.Fatalf("first page = %d, %v", len(first), err)
	}
	second, err := store.BackupOperations(ctx, first[len(first)-1].ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[uuid.UUID]bool{}
	for _, operation := range append(first, second...) {
		if seen[operation.ID] {
			t.Fatalf("operation %s repeated across pages", operation.ID)
		}
		seen[operation.ID] = true
	}
	if len(second) == 0 {
		t.Fatal("second page is empty")
	}
}
