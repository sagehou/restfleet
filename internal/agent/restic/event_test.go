package restic

import (
	"strings"
	"testing"
)

func TestParseSummaryKeepsNewFilesAndAddedBytes(t *testing.T) {
	snapshot := strings.Repeat("a", 64)
	event, ok, err := ParseEvent([]byte(`{"message_type":"summary","snapshot_id":"`+snapshot+`","files_new":3,"data_added":42,"total_files_processed":9}`))
	if err != nil || !ok {
		t.Fatalf("summary parse = %v, %v", ok, err)
	}
	if event.Type != "summary" || event.SnapshotID != snapshot ||
		event.FilesNew != 3 || event.DataAdded != 42 || event.FilesDone != 9 {
		t.Fatalf("summary stats = %+v", event)
	}
}

func TestParseEventRejectsMalformedRequiredValuesWithoutEchoingInput(t *testing.T) {
	for _, line := range []string{
		`{"message_type":"summary"}`,
		`{"message_type":"summary","files_new":0,"data_added":-1}`,
		`{"message_type":"summary","files_new":0,"data_added":0,"snapshot_id":"secret-canary"}`,
		`{"message_type":"status","percent_done":2}`,
		`{"message_type":"status","bytes_done":-1}`,
		`{"message_type":"status","files_done":"secret-canary"}`,
		`{"message_type":`,
	} {
		if _, _, err := ParseEvent([]byte(line)); err == nil || strings.Contains(err.Error(), "secret-canary") {
			t.Fatalf("unsafe parser result for %q: %v", line, err)
		}
	}
	if _, ok, err := ParseEvent([]byte(`{"message_type":"future","files_done":"unknown-format"}`)); err != nil || ok {
		t.Fatalf("unknown message should be ignored: %v, %v", ok, err)
	}
	if _, ok, err := ParseEvent([]byte(`{"message_type":"summary","files_new":0,"data_added":0}`)); err != nil || !ok {
		t.Fatalf("unchanged summary rejected: %v, %v", ok, err)
	}
}
