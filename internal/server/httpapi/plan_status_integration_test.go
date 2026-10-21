package httpapi

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sagehou/restfleet/internal/domain"
	control "github.com/sagehou/restfleet/internal/server"
)

func TestPlanStatusTransitionsDoNotCreateRevisions(t *testing.T) {
	_, pool, _, enrolled, repository, c := deliveryFixture(t, "onedrive")
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	var userID uuid.UUID
	if err := pool.QueryRow(ctx, "select id from users limit 1").Scan(&userID); err != nil {
		t.Fatal(err)
	}
	templateID, planID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	config := domain.TemplateConfig{
		Paths: []string{"/tmp"}, ScheduleCron: "0 * * * *", ScheduleTimezone: "UTC",
		MisfireGraceSeconds: 60, EnabledByDefault: true,
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
		effective_config,config_hash,status,desired_revision,accepted_revision,created_at,updated_at)
		values($1,$2,$3,$4,$5,$6,1,$7::jsonb,$8,'DRAFT',1,0,$9,$9)`,
		planID, planID.String(), repository.HostId, enrolled.AgentId, repository.Id, templateID, string(raw), hash, now); err != nil {
		t.Fatal(err)
	}

	var agentRevision int64
	if err := pool.QueryRow(ctx, "select desired_revision from agents where id=$1", enrolled.AgentId).Scan(&agentRevision); err != nil {
		t.Fatal(err)
	}
	if agentRevision == 0 {
		state, stateErr := domain.NewDefaultDesiredState(enrolled.AgentId, 1, now)
		if stateErr != nil {
			t.Fatal(stateErr)
		}
		canonical, stateErr := state.CanonicalJSON()
		if stateErr != nil {
			t.Fatal(stateErr)
		}
		if _, stateErr = pool.Exec(ctx, `insert into agent_desired_states(agent_id,revision,generated_at,config_hash,config_json,created_at)
			values($1,1,$2,$3,$4::jsonb,$2)`, enrolled.AgentId, now, state.ConfigHash, string(canonical)); stateErr != nil {
			t.Fatal(stateErr)
		}
		if _, stateErr = pool.Exec(ctx, "update agents set desired_revision=1 where id=$1", enrolled.AgentId); stateErr != nil {
			t.Fatal(stateErr)
		}
	}

	actor, meta := domain.User{ID: userID, Role: domain.RoleAdmin}, control.RequestMeta{}
	if _, err := c.PausePlan(ctx, planID, 1, actor, meta); !errors.Is(err, domain.ErrPlanStateInvalid) {
		t.Fatalf("pause DRAFT = %v, want ErrPlanStateInvalid", err)
	}
	applied, err := c.ApplyPlan(ctx, planID, 1, actor, meta)
	if err != nil || applied.Status != "ACTIVE" {
		t.Fatalf("apply = %+v, %v", applied, err)
	}
	paused, err := c.PausePlan(ctx, planID, 1, actor, meta)
	if err != nil || paused.Status != "PAUSED" {
		t.Fatalf("pause = %+v, %v", paused, err)
	}
	resumed, err := c.ResumePlan(ctx, planID, 1, actor, meta)
	if err != nil || resumed.Status != "ACTIVE" {
		t.Fatalf("resume = %+v, %v", resumed, err)
	}

	var desired int64
	var revisions int
	if err := pool.QueryRow(ctx, `select (select desired_revision from plans where id=$1),
		(select count(*) from plan_revisions where plan_id=$1)`, planID).Scan(&desired, &revisions); err != nil {
		t.Fatal(err)
	}
	if desired != 1 || revisions != 1 {
		t.Fatalf("status transition changed revisions: desired=%d revisions=%d", desired, revisions)
	}
}
