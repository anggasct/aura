package httpchat

import (
	"math"
	"sync"
	"time"
)

const maxTrackedBuckets = 4096

type bucket struct {
	tokens   float64
	lastSeen time.Time
}

type rateLimiter struct {
	capacity float64
	perSec   float64

	mu      sync.Mutex
	buckets map[string]*bucket
}

func newRateLimiter(requestsPerMinute int) *rateLimiter {
	return &rateLimiter{
		capacity: float64(requestsPerMinute),
		perSec:   float64(requestsPerMinute) / time.Minute.Seconds(),
		buckets:  make(map[string]*bucket),
	}
}

func (l *rateLimiter) allow(keyID string, now time.Time) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	current, tracked := l.buckets[keyID]
	if !tracked {
		if len(l.buckets) >= maxTrackedBuckets {
			l.evictLocked(now)
		}
		current = &bucket{tokens: l.capacity, lastSeen: now}
		l.buckets[keyID] = current
	}
	l.refillLocked(current, now)
	if current.tokens >= 1 {
		current.tokens--
		return true, 0
	}
	wait := (1 - current.tokens) / l.perSec
	return false, time.Duration(math.Ceil(wait)) * time.Second
}

func (l *rateLimiter) refillLocked(b *bucket, now time.Time) {
	b.tokens = l.projected(b, now)
	if now.After(b.lastSeen) {
		b.lastSeen = now
	}
}

func (l *rateLimiter) projected(b *bucket, now time.Time) float64 {
	elapsed := max(now.Sub(b.lastSeen), 0)
	return math.Min(l.capacity, b.tokens+elapsed.Seconds()*l.perSec)
}

func (l *rateLimiter) evictLocked(now time.Time) {
	var oldestKey string
	var oldest time.Time
	for key, b := range l.buckets {
		if l.projected(b, now) >= l.capacity {
			delete(l.buckets, key)
			continue
		}
		if oldestKey == "" || b.lastSeen.Before(oldest) {
			oldestKey, oldest = key, b.lastSeen
		}
	}
	if len(l.buckets) >= maxTrackedBuckets {
		delete(l.buckets, oldestKey)
	}
}

type slotTable struct {
	limit int

	mu     sync.Mutex
	active map[string]int
}

func newSlotTable(limit int) *slotTable {
	return &slotTable{limit: limit, active: make(map[string]int)}
}

func (t *slotTable) acquire(keyID string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.active[keyID] >= t.limit {
		return false
	}
	t.active[keyID]++
	return true
}

func (t *slotTable) release(keyID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.active[keyID] <= 1 {
		delete(t.active, keyID)
		return
	}
	t.active[keyID]--
}
