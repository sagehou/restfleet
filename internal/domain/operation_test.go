package domain

import "testing"

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
