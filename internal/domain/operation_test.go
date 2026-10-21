package domain

import (
	"testing"

	"github.com/google/uuid"
)

func TestOperationTransitions(t *testing.T) {
	edges := map[string][]string{
		"QUEUED":           {"DISPATCHED", "REJECTED", "CANCELED"},
		"DISPATCHED":       {"ACKNOWLEDGED", "LOST", "FAILED"},
		"ACKNOWLEDGED":     {"RUNNING"},
		"RUNNING":          {"SUCCEEDED", "SUCCEEDED_WITH_WARNINGS", "FAILED", "CANCEL_REQUESTED", "TIMED_OUT"},
		"CANCEL_REQUESTED": {"CANCELED"},
	}
	states := []string{"QUEUED", "DISPATCHED", "ACKNOWLEDGED", "RUNNING", "CANCEL_REQUESTED", "SUCCEEDED", "SUCCEEDED_WITH_WARNINGS", "FAILED", "CANCELED", "TIMED_OUT", "LOST", "REJECTED", "UNKNOWN", ""}
	for _, from := range states {
		for _, to := range states {
			want := false
			for _, allowed := range edges[from] {
				want = want || allowed == to
			}
			if got := ValidateOperationTransition(from, to) == nil; got != want {
				t.Fatalf("%s -> %s = %v", from, to, got)
			}
			if OperationTerminal(from) && ValidateOperationTransition(from, to) == nil {
				t.Fatal("terminal reopened")
			}
		}
	}
	if OperationTerminal("UNKNOWN") || OperationTerminal("") {
		t.Fatal("unknown state treated as success")
	}
	if ValidCredentialTestCode("canary-secret") {
		t.Fatal("arbitrary error persisted")
	}
}

func TestValidOperationIDAcceptsScheduledKey(t *testing.T) {
	scheduled := uuid.NewSHA1(uuid.NameSpaceOID, []byte("plan|2026-10-21T00:00:00Z"))
	manual := uuid.Must(uuid.NewV7())
	for _, id := range []uuid.UUID{scheduled, manual} {
		if !ValidOperationID(id) {
			t.Fatalf("valid operation ID %s rejected", id)
		}
	}
	for _, id := range []uuid.UUID{uuid.Nil, uuid.New(), uuid.NewMD5(uuid.NameSpaceOID, []byte("v3"))} {
		if ValidOperationID(id) {
			t.Fatalf("non-operation UUID %s accepted", id)
		}
	}
}
