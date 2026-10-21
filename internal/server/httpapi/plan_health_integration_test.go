package httpapi

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sagehou/restfleet/internal/domain"
	control "github.com/sagehou/restfleet/internal/server"
)

func TestPlanHealthNeverRunPauseOverdueAndHeartbeat(t *testing.T) {
	store, pool, _, enrolled, repository, c := deliveryFixture(t, "onedrive")
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	var userID uuid.UUID
	if err := pool.QueryRow(ctx, "select id from users limit 1").Scan(&userID); err != nil {
		t.Fatal(err)
	}
	templateID, planID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	config := domain.TemplateConfig{
		Paths: []string{"/tmp"}, ScheduleCron: "0 * * * *", ScheduleTimezone: "UTC",
		MisfireGraceSeconds: 0, EnabledByDefault: true,
	}
	raw, err := config.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	hash, err := config.ConfigHash()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `insert into templates(id,name,current_revision,created_at,updated_at)
		values($1,$2,1,$3,$3)`, templateID, templateID.String(), now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `insert into template_revisions(template_id,revision,config_json,config_hash,created_by,created_at)
		values($1,1,$2::jsonb,$3,$4,$5)`, templateID, string(raw), hash, userID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `insert into plans(id,name,host_id,agent_id,repository_id,template_id,template_revision,
		effective_config,config_hash,status,desired_revision,accepted_revision,backup_health,created_at,updated_at)
		values($1,$2,$3,$4,$5,$6,1,$7::jsonb,$8,'ACTIVE',1,0,'UNKNOWN',$9,$9)`,
		planID, planID.String(), repository.HostId, enrolled.AgentId, repository.Id, templateID, string(raw), hash, now); err != nil {
		t.Fatal(err)
	}

	next := now.Add(time.Hour)
	if _, err := store.RecordAgentHeartbeat(ctx, domain.AgentHeartbeat{
		AgentID: enrolled.AgentId, BootID: "health-boot", LocalTime: now, ReceivedAt: now,
		NextRuns: []domain.AgentNextRun{{PlanID: planID, ScheduledAt: next}},
	}); err != nil {
		t.Fatal(err)
	}
	var nextRun time.Time
	if err := pool.QueryRow(ctx, "select next_run_at from plans where id=$1", planID).Scan(&nextRun); err != nil {
		t.Fatal(err)
	}
	if !nextRun.Equal(next) {
		t.Fatalf("next_run_at = %s, want %s", nextRun, next)
	}

	if _, err := pool.Exec(ctx, `update plans set next_run_at=$2,last_operation_id=$3,backup_health='NEVER_RUN'
		where id=$1`, planID, now.Add(-time.Hour), uuid.Must(uuid.NewV7())); err != nil {
		t.Fatal(err)
	}
	if closed, err := store.ReconcilePlanHealth(ctx, now); err != nil || closed != 1 {
		t.Fatalf("overdue reconciliation = %d, %v", closed, err)
	}
	var health string
	if err := pool.QueryRow(ctx, "select backup_health from plans where id=$1", planID).Scan(&health); err != nil {
		t.Fatal(err)
	}
	if health != "OVERDUE" {
		t.Fatalf("health = %s, want OVERDUE", health)
	}

	actor, meta := domain.User{ID: userID, Role: domain.RoleAdmin}, control.RequestMeta{}
	if _, err := c.PausePlan(ctx, planID, 1, actor, meta); err != nil {
		t.Fatal(err)
	}
	var pausedNext *time.Time
	if err := pool.QueryRow(ctx, "select backup_health,next_run_at from plans where id=$1", planID).Scan(&health, &pausedNext); err != nil {
		t.Fatal(err)
	}
	if health != "PAUSED" || pausedNext != nil {
		t.Fatalf("paused plan = %s/%v, want PAUSED/nil", health, pausedNext)
	}
	if _, err := c.ResumePlan(ctx, planID, 1, actor, meta); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, "select backup_health from plans where id=$1", planID).Scan(&health); err != nil {
		t.Fatal(err)
	}
	if health != "NEVER_RUN" {
		t.Fatalf("resumed plan health = %s, want NEVER_RUN", health)
	}
}
