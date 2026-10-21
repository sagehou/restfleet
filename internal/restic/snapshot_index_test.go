package restic

import (
	"strings"
	"testing"
)

func TestParseSnapshotIndex(t *testing.T) {
	id := strings.Repeat("a", 64)
	tree := strings.Repeat("b", 64)
	raw := `[{"id":"` + id + `","tree":"` + tree + `","time":"2026-10-21T10:00:00.123456789Z",` +
		`"paths":["/srv"],"tags":["restfleet:managed","restfleet:plan:019a0000-0000-7000-8000-000000000001"],` +
		`"program_version":"restic 0.19.1","summary":{"files_new":3,"data_added":4096}}]`
	result, err := parseSnapshotIndex([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Entries) != 1 {
		t.Fatalf("entries = %d", len(result.Entries))
	}
	entry := result.Entries[0]
	if entry.SnapshotID != id || entry.TreeID != tree || entry.FilesNew != 3 || entry.BytesAdded != 4096 ||
		entry.CapturedAt.IsZero() || len(entry.Tags) != 2 || len(entry.Paths) != 1 {
		t.Fatalf("entry = %+v", entry)
	}
}

func TestParseSnapshotIndexRejectsMalformed(t *testing.T) {
	for _, raw := range []string{
		`{}`,
		`[{"id":"short","tree":"` + strings.Repeat("b", 64) + `","time":"2026-10-21T10:00:00Z"}]`,
		`[{"id":"` + strings.Repeat("a", 64) + `","tree":"short","time":"2026-10-21T10:00:00Z"}]`,
		`[{"id":"` + strings.Repeat("a", 64) + `","tree":"` + strings.Repeat("b", 64) + `","time":"not-a-time"}]`,
	} {
		if _, err := parseSnapshotIndex([]byte(raw)); err == nil {
			t.Fatalf("malformed index accepted: %s", raw)
		}
	}
}
