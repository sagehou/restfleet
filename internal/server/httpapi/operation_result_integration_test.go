package httpapi

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sagehou/restfleet/internal/domain"
)

func TestOperationTerminalGuardAndRejectedQueue(t *testing.T) {
	store, pool, _, enrolled, repository, _ := deliveryFixture(t, "onedrive")
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	var userID uuid.UUID
	if err := pool.QueryRow(ctx, "select id from users limit 1").Scan(&userID); err != nil {
		t.Fatal(err)
	}
	templateID, planID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	hash := strings.Repeat("a", 64)
	if _, err := pool.Exec(ctx, `insert into templates(id,name,current_revision,created_at,updated_at)
		values($1,'operation-guard',1,$2,$2)`, templateID, now); err != nil {
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
	if err := store.RejectManualOperation(ctx, queued, now); err != nil {
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
}
