package webhook

import (
	"sync"
	"time"
)

// RateLimiter is a fixed-window per-minute counter keyed by both signing key
// identifier and client address, so one compromised key and one flooding
// address are each contained independently. It has no goroutine and no
// ticker: windows advance lazily on Allow, driven by the injected clock.
type RateLimiter struct {
	limit int
	now   func() time.Time

	mu      sync.Mutex
	buckets map[string]time.Time
	counts  map[string]int
}

// NewRateLimiter allows limit requests per minute per key. A limit below 1 is
// rejected: a limiter that admits nothing is a configuration error, not a
// mode to run in.
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

// Allow reports whether one request from keyID at remoteAddr fits the
// per-minute budget. Both scopes must pass; the rejected scope is returned
// so callers log which budget was exhausted without logging credentials.
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

// pruneLocked drops windows that ended more than a minute ago so the maps
// stay bounded by the distinct keys and addresses seen in the last two
// minutes, not by process lifetime.
func (l *RateLimiter) pruneLocked(now time.Time) {
	for scope, window := range l.buckets {
		if now.Sub(window) > time.Minute {
			delete(l.buckets, scope)
			delete(l.counts, scope)
		}
	}
}
