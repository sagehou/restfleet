package restic

import "testing"

func TestParseSummaryKeepsNewFilesAndAddedBytes(t *testing.T) {
	event, ok, err := ParseEvent([]byte(`{"message_type":"summary","snapshot_id":"snapshot","files_new":3,"data_added":42,"files_done":9}`))
	if err != nil || !ok {
		t.Fatalf("summary parse = %v, %v", ok, err)
	}
	if event.Type != "summary" || event.SnapshotID != "snapshot" ||
		event.FilesNew != 3 || event.DataAdded != 42 || event.FilesDone != 9 {
		t.Fatalf("summary stats = %+v", event)
	}
}
