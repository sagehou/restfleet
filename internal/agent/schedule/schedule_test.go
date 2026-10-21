package schedule

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestNextScheduledSkipsNonexistentDSTTime(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	scheduler, err := New(uuid.Must(uuid.NewV7()), Spec{Cron: "30 2 * * *", Timezone: "America/New_York"})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 3, 8, 0, 0, 0, 0, loc)
	next, ok := scheduler.NextScheduled(start)
	if !ok {
		t.Fatal("no next scheduled time")
	}
	want := time.Date(2026, 3, 9, 2, 30, 0, 0, loc)
	if !next.Equal(want) {
		t.Fatalf("next = %s, want %s (02:30 does not exist on 2026-03-08)", next, want)
	}
}
