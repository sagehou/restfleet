package httpapi

import (
	"sync"
	"time"
)

const maxRateLimitEntries = 4096

type rateWindow struct {
	start time.Time
	count int
}

type rateLimiter struct {
	mu      sync.Mutex
	limit   int
	window  time.Duration
	entries map[string]rateWindow
	now     func() time.Time
}

func newRateLimiter(limit int, window time.Duration) *rateLimiter {
	return &rateLimiter{
		limit:   limit,
		window:  window,
		entries: make(map[string]rateWindow),
		now:     time.Now,
	}
}

func (l *rateLimiter) allow(keys ...string) bool {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()

	if len(l.entries) >= maxRateLimitEntries {
		for key, entry := range l.entries {
			if now.Sub(entry.start) >= l.window {
				delete(l.entries, key)
			}
		}
	}
	newKeys := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		if _, ok := l.entries[key]; !ok {
			newKeys[key] = struct{}{}
		}
	}
	if len(l.entries)+len(newKeys) > maxRateLimitEntries {
		return false
	}

	for _, key := range keys {
		entry, ok := l.entries[key]
		if ok && now.Sub(entry.start) < l.window && entry.count >= l.limit {
			return false
		}
	}
	for _, key := range keys {
		entry, ok := l.entries[key]
		if !ok || now.Sub(entry.start) >= l.window {
			l.entries[key] = rateWindow{start: now, count: 1}
			continue
		}
		entry.count++
		l.entries[key] = entry
	}
	return true
}
