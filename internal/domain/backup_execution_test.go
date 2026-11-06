package domain

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"
)

func TestBackupExecutionValidatesBoundsAndOverrides(t *testing.T) {
	base:=TemplateConfig{Paths:[]string{"/data"},ScheduleCron:"0 * * * *",ScheduleTimezone:"UTC"}
	for _,patch:=range []string{
		`{"retry_policy":{"max_attempts":11}}`,
		`{"retry_policy":{"max_attempts":1.5}}`,
		`{"retry_policy":{"initial_backoff_seconds":2000,"max_backoff_seconds":1000}}`,
		`{"retry_policy":{"unknown":1}}`,
		`{"resource_limits":{"cpu":50}}`,
		`{"resource_limits":{"max_duration_seconds":86401}}`,
		`{"resource_limits":{"read_concurrency":-1}}`,
		`{"pre_hook_ids":["00000000-0000-0000-0000-000000000000"]}`,
	} {
		if _,err:=MergePlanConfig(base,json.RawMessage(patch));err==nil {t.Fatalf("unsafe execution config accepted: %s",patch)}
	}
	if _,err:=MergePlanConfig(base,json.RawMessage(`{"retry_policy":{"max_attempts":3},"resource_limits":{"limit_upload_kib":1024,"read_concurrency":2}}`));err!=nil {t.Fatal(err)}
	id:=uuid.Must(uuid.NewV7())
	base.PreHookIDs=[]uuid.UUID{id,id}
	if base.Validate()==nil {t.Fatal("duplicate Hook accepted")}
	p:=BackupRetryPolicy{MaxAttempts:3,InitialBackoffSeconds:60,MaxBackoffSeconds:120}
	if p.Attempts()!=3 || p.Backoff(1)!=60 || p.Backoff(2)!=120 || p.Backoff(9)!=120 {t.Fatal("retry backoff is not capped")}
}
