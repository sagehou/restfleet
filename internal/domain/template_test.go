package domain

import "testing"

func TestTemplateConfigRejectsNonCanonicalPaths(t *testing.T) {
	config := TemplateConfig{Paths: []string{"/tmp"}, ScheduleCron: "0 * * * *", ScheduleTimezone: "UTC"}
	if err := config.Validate(); err != nil {
		t.Fatalf("canonical path rejected: %v", err)
	}
	for _, path := range []string{"relative", "/tmp/../etc", "/tmp/", "/tmp\nx", "/tmp\rx", "/tmp\x00", ""} {
		bad := config
		bad.Paths = []string{path}
		if bad.Validate() == nil {
			t.Fatalf("non-canonical path %q accepted", path)
		}
	}
	for _, pattern := range []string{"x\ny", "x\rx", "x\x00"} {
		bad := config
		bad.ExcludePatterns = []string{pattern}
		if bad.Validate() == nil {
			t.Fatalf("unsafe exclude pattern %q accepted", pattern)
		}
	}
}
