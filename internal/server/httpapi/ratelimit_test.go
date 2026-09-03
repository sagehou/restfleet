package httpapi

import (
	"fmt"
	"testing"
	"time"
)

func TestRateLimiterUsesAllKeys(t *testing.T) {
	now := time.Date(2026, 9, 3, 8, 0, 0, 0, time.UTC)
	limiter := newRateLimiter(2, time.Minute)
	limiter.now = func() time.Time { return now }

	if !limiter.allow("ip:a", "account:x") || !limiter.allow("ip:a", "account:y") {
		t.Fatal("requests within limit were rejected")
	}
	if limiter.allow("ip:a", "account:z") {
		t.Fatal("IP limit was not enforced")
	}
	now = now.Add(time.Minute)
	if !limiter.allow("ip:a", "account:z") {
		t.Fatal("expired window did not reset")
	}
}

func TestRateLimiterHasAHardEntryCap(t *testing.T) {
	now := time.Date(2026, 9, 3, 8, 0, 0, 0, time.UTC)
	limiter := newRateLimiter(2, time.Minute)
	limiter.now = func() time.Time { return now }
	for index := range maxRateLimitEntries {
		if !limiter.allow(fmt.Sprintf("ip:%d", index)) {
			t.Fatalf("entry %d was rejected before reaching the cap", index)
		}
	}
	if limiter.allow("ip:overflow") {
		t.Fatal("new key was accepted beyond the entry cap")
	}
	if len(limiter.entries) != maxRateLimitEntries {
		t.Fatalf("entry count = %d, want %d", len(limiter.entries), maxRateLimitEntries)
	}
}
