package buildinfo

import "testing"

func TestString(t *testing.T) {
	previousVersion, previousCommit, previousDate := Version, Commit, Date
	t.Cleanup(func() { Version, Commit, Date = previousVersion, previousCommit, previousDate })

	Version, Commit, Date = "1.2.3", "abc123", "2026-09-03T00:00:00Z"
	if got, want := String(), "1.2.3 (commit=abc123, built=2026-09-03T00:00:00Z)"; got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
}
