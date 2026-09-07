package webhook

import (
	"sync"
	"time"
)

type RateLimiter struct {
	limit int
	now   func() time.Time

	mu      sync.Mutex
	buckets map[string]time.Time
	counts  map[string]int
}

func NewRateLimiter(limit int, now func() time.Time) (*RateLimiter, error) {
	if limit < 1 {
		return nil, Errorf(ErrorCodeInvalidArgument, "rate limit must be at least 1")
	}
	if now == nil {
		return nil, Errorf(ErrorCodeInvalidArgument, "clock must not be nil")
	}
	return &RateLimiter{
		limit:   limit,
		now:     now,
		buckets: make(map[string]time.Time),
		counts:  make(map[string]int),
	}, nil
}

func (l *RateLimiter) Allow(keyID, remoteAddr string) (ok bool, exhaustedScope string) {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pruneLocked(now)
	if !l.withinLocked("key:"+keyID, now) {
		return false, "key"
	}
	if !l.withinLocked("addr:"+remoteAddr, now) {
		return false, "addr"
	}
	return true, ""
}

func (l *RateLimiter) withinLocked(scope string, now time.Time) bool {
	window := l.buckets[scope]
	if window.IsZero() || !now.Before(window) {
		l.buckets[scope] = now.Add(time.Minute)
		l.counts[scope] = 0
	}
	if l.counts[scope] >= l.limit {
		return false
	}
	l.counts[scope]++
	return true
}

func (l *RateLimiter) pruneLocked(now time.Time) {
	for scope, window := range l.buckets {
		if now.Sub(window) > time.Minute {
			delete(l.buckets, scope)
			delete(l.counts, scope)
		}
	}
}
