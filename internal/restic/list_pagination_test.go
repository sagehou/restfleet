package restic

import (
	"strings"
	"testing"
)

func TestListPagination(t *testing.T) {
	tree := strings.Repeat("a", 64)
	var input strings.Builder
	input.WriteString(`{"message_type":"snapshot","tree":"` + tree + `"}` + "\n")
	for _, name := range []string{"a", "b", "c"} {
		input.WriteString(`{"message_type":"node","name":"` + name + `","path":"/` + name + `","type":"file","mtime":"2026-10-21T00:00:00Z"}` + "\n")
	}
	var first ListResult
	if err := parseListJSON(strings.NewReader(input.String()), "/", 10, 2, "", &first); err != nil {
		t.Fatal(err)
	}
	if len(first.Entries) != 2 || first.NextCursor != "/b" || !first.Truncated {
		t.Fatalf("first page = %+v", first)
	}
	var second ListResult
	if err := parseListJSON(strings.NewReader(input.String()), "/", 10, 2, first.NextCursor, &second); err != nil {
		t.Fatal(err)
	}
	if len(second.Entries) != 1 || second.Entries[0].Path != "/c" || second.NextCursor != "" || second.Truncated {
		t.Fatalf("second page = %+v", second)
	}
}
