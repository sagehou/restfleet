package agent

import (
	"bytes"
	"strings"
	"testing"
)

func TestBackupResultClassification(t *testing.T) {
	for _, test := range []struct {
		exit         int
		summary      bool
		snapshot     string
		status, code string
	}{
		{0, true, "", OperationSucceeded, ""},
		{0, false, "", OperationFailed, "INVALID_ENGINE_OUTPUT"},
		{3, true, strings.Repeat("a", 64), OperationSucceededWarnings, "PARTIAL_READ"},
		{3, false, strings.Repeat("a", 64), OperationFailed, "INVALID_ENGINE_OUTPUT"},
		{10, false, "", OperationFailed, "REPOSITORY_MISSING"},
		{11, false, "", OperationFailed, "REPOSITORY_LOCKED"},
		{12, false, "", OperationFailed, "WRONG_REPOSITORY_PASSWORD"},
		{99, true, strings.Repeat("a", 64), OperationFailed, "UNKNOWN_EXIT_CODE"},
	} {
		got := classify(test.exit, test.snapshot, test.summary, &progressSink{})
		if got.Status != test.status || got.ErrorCode != test.code {
			t.Fatalf("exit %d summary %v: %+v", test.exit, test.summary, got)
		}
	}
}

func TestProgressSinkBoundsAndFinalLine(t *testing.T) {
	canceled := false
	sink := &progressSink{cancel: func() { canceled = true }}
	_, _ = sink.Write(bytes.Repeat([]byte("x"), 1<<20))
	_, _ = sink.Write([]byte("secret-canary"))
	if !canceled || sink.errorCode != "INVALID_ENGINE_OUTPUT" || len(sink.pendingLine) != 0 {
		t.Fatal("oversized engine output was not stopped and released")
	}
	sink = &progressSink{}
	for _, chunk := range []string{`{"message_type":"sum`, `mary","files_new":2,"data_added":42,"total_files_processed":2,"total_bytes_processed":42}`} {
		_, _ = sink.Write([]byte(chunk))
	}
	sink.finish()
	if sink.errorCode != "" || !sink.sawSummary || sink.filesNew != 2 || sink.dataAdded != 42 {
		t.Fatalf("split unterminated summary lost: %+v", sink)
	}
	sink = &progressSink{}
	_, _ = sink.Write([]byte("{broken}\n"))
	if sink.errorCode != "INVALID_ENGINE_OUTPUT" {
		t.Fatal("malformed engine output was silently ignored")
	}
}

func TestBackupWarningsCountStderrWithoutKeepingRawErrors(t *testing.T) {
	sink := &backupWarningSink{}
	for _, chunk := range []string{
		"fatal engine text secret-canary\n{\"message_type\":\"err",
		"or\",\"error\":{\"message\":\"secret-canary\"},\"item\":\"/private/file\"}\n",
		"{\"message_type\":\"new_type\"}\n{broken}\n",
		"{\"message_type\":\"error\"}",
	} {
		_, _ = sink.Write([]byte(chunk))
	}
	sink.finish()
	if sink.count != 2 || len(sink.pending) != 0 {
		t.Fatal("typed stderr warnings were lost or raw errors retained")
	}
	_, _ = sink.Write(bytes.Repeat([]byte("x"), (1<<20)+1))
	if len(sink.pending) != 0 || !sink.discarding {
		t.Fatal("oversized stderr event exceeded its bound")
	}
	_, _ = sink.Write([]byte("{\"message_type\":\"error\"}\n{\"message_type\":\"error\"}"))
	sink.finish()
	if sink.count != 3 || len(sink.pending) != 0 {
		t.Fatal("stderr parser did not resume after the discarded line")
	}
}
