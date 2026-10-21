package restic

import (
	"errors"
	"strings"
	"testing"
)

func TestListRejectsOversizedEntryName(t *testing.T) {
	name := strings.Repeat("n", 256)
	input := `{"message_type":"snapshot","tree":"` + strings.Repeat("a", 64) + `"}` + "\n" +
		`{"message_type":"node","name":"` + name + `","path":"/` + name + `","type":"file","mtime":"2026-10-21T00:00:00Z"}`
	var result ListResult
	if err := parseListJSON(strings.NewReader(input), "/", 10, 10, &result); !errors.Is(err, ErrInvalidOutput) {
		t.Fatalf("oversized entry name = %v, want ErrInvalidOutput", err)
	}
}
