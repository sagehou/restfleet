package restic

import "testing"

func TestPrepareRejectsNonCanonicalPathsAndPatterns(t *testing.T) {
	for _, path := range []string{"relative", "/tmp/../etc", "/tmp/", "/tmp\nx", "/tmp\rx", "/tmp\x00"} {
		if _, err := Prepare([]string{path}, nil); err == nil {
			t.Fatalf("non-canonical path %q accepted", path)
		}
	}
	for _, pattern := range []string{"x\ny", "x\rx", "x\x00"} {
		if _, err := Prepare([]string{"/tmp"}, []string{pattern}); err == nil {
			t.Fatalf("unsafe exclude pattern %q accepted", pattern)
		}
	}
}
