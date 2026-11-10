package httpchat

import (
	"fmt"
	"testing"
	"time"
)

func TestRateLimiterBucketsAreBounded(t *testing.T) {
	limiter := newRateLimiter(10)
	now := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	for i := range maxTrackedBuckets + 100 {
		if ok, _ := limiter.allow(fmt.Sprintf("key-%d", i), now.Add(time.Duration(i)*time.Millisecond)); !ok {
			t.Fatalf("first request of key %d was limited", i)
		}
	}
	if got := len(limiter.buckets); got > maxTrackedBuckets {
		t.Fatalf("tracked buckets = %d, want at most %d", got, maxTrackedBuckets)
	}
}

func TestRateLimiterPrunesRefilledBucketsBeforeEvictingBusyOnes(t *testing.T) {
	limiter := newRateLimiter(60)
	now := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	for i := range maxTrackedBuckets {
		limiter.allow(fmt.Sprintf("key-%d", i), now)
	}
	later := now.Add(time.Minute)
	limiter.allow("fresh", later)
	if got := len(limiter.buckets); got != 1 {
		t.Fatalf("tracked buckets = %d, want only the fresh key after the others refilled", got)
	}
}

func TestRateLimiterEvictsTheLeastRecentlySeenBusyBucket(t *testing.T) {
	limiter := newRateLimiter(1)
	now := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	for i := range maxTrackedBuckets {
		limiter.allow(fmt.Sprintf("key-%d", i), now.Add(time.Duration(i)*time.Millisecond))
	}
	limiter.allow("newcomer", now.Add(time.Second))
	if _, kept := limiter.buckets["key-0"]; kept {
		t.Error("the least recently seen bucket survived eviction")
	}
	if _, kept := limiter.buckets["newcomer"]; !kept || len(limiter.buckets) != maxTrackedBuckets {
		t.Errorf("tracked buckets = %d, newcomer kept = %v", len(limiter.buckets), kept)
	}
}

func TestRateLimiterRetryAfterRoundsUp(t *testing.T) {
	limiter := newRateLimiter(40)
	now := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	for range 40 {
		if ok, _ := limiter.allow("key", now); !ok {
			t.Fatal("request within the burst was limited")
		}
	}
	ok, wait := limiter.allow("key", now)
	if ok || wait != 2*time.Second {
		t.Fatalf("allow = %v, %v; want limited with a 2s wait (1.5s rounded up)", ok, wait)
	}
}

func TestSlotTableBoundsAndReleases(t *testing.T) {
	slots := newSlotTable(2)
	first, second, third := slots.acquire("a"), slots.acquire("a"), slots.acquire("a")
	if !first || !second || third {
		t.Fatal("slot table did not enforce its limit of 2")
	}
	if !slots.acquire("b") {
		t.Fatal("another key was refused a slot")
	}
	slots.release("a")
	if !slots.acquire("a") {
		t.Fatal("released slot was not reusable")
	}
	slots.release("a")
	slots.release("a")
	slots.release("b")
	if len(slots.active) != 0 {
		t.Fatalf("slot table keeps %d idle entries", len(slots.active))
	}
}
