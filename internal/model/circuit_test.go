package model

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/config"
)

type memoryCheckpointStore struct {
	mu          sync.Mutex
	checkpoints map[string]CircuitCheckpoint
}

func newMemoryCheckpointStore() *memoryCheckpointStore {
	return &memoryCheckpointStore{
		checkpoints: make(map[string]CircuitCheckpoint),
	}
}

func (s *memoryCheckpointStore) Save(_ context.Context, cp *CircuitCheckpoint) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cp != nil {
		s.checkpoints[cp.CircuitKey] = *cp
	}
	return nil
}

func (s *memoryCheckpointStore) Load(_ context.Context) ([]CircuitCheckpoint, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var list []CircuitCheckpoint
	for _, cp := range s.checkpoints {
		list = append(list, cp)
	}
	return list, nil
}

func (s *memoryCheckpointStore) Delete(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.checkpoints, key)
	return nil
}

func TestCircuitManager_ClosedToOpenAndHalfOpenProbe(t *testing.T) {
	currTime := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	nowFn := func() time.Time { return currTime }

	mgr := NewCircuitManager(nowFn, nil)
	key := FormatCircuitKey("openai-primary", "https://api.openai.com")

	policy := CircuitPolicy{
		FailureThreshold: 3,
		OpenDuration:     5 * time.Minute,
		MaxOpenDuration:  30 * time.Minute,
	}
	mgr.Register("openai-primary", "https://api.openai.com", "digest123", policy)

	allowed, isProbe := mgr.Allow(key)
	if !allowed || isProbe {
		t.Fatalf("Allow() = (%v, %v); want (true, false)", allowed, isProbe)
	}

	mgr.RecordFailure(t.Context(), key, ErrorClassTransient)
	mgr.RecordFailure(t.Context(), key, ErrorClassRateLimited)

	allowed, isProbe = mgr.Allow(key)
	if !allowed || isProbe {
		t.Fatalf("Allow() after 2 failures = (%v, %v); want (true, false)", allowed, isProbe)
	}

	mgr.RecordFailure(t.Context(), key, ErrorClassOverloaded)

	allowed, isProbe = mgr.Allow(key)
	if allowed {
		t.Fatalf("Allow() on open circuit = (%v, %v); want false", allowed, isProbe)
	}

	currTime = currTime.Add(4 * time.Minute)
	allowed, isProbe = mgr.Allow(key)
	if allowed {
		t.Fatalf("Allow() at 4m = (%v, %v); want false", allowed, isProbe)
	}

	currTime = currTime.Add(2 * time.Minute)
	allowed, isProbe = mgr.Allow(key)
	if !allowed || !isProbe {
		t.Fatalf("Allow() after open duration = (%v, %v); want (true, true)", allowed, isProbe)
	}

	allowed2, isProbe2 := mgr.Allow(key)
	if allowed2 || isProbe2 {
		t.Fatalf("Concurrent Allow() during probe = (%v, %v); want (false, false)", allowed2, isProbe2)
	}

	mgr.RecordSuccess(t.Context(), key)

	allowed, isProbe = mgr.Allow(key)
	if !allowed || isProbe {
		t.Fatalf("Allow() after probe success = (%v, %v); want (true, false)", allowed, isProbe)
	}
}

func TestCircuitManager_FailedProbeBackoff(t *testing.T) {
	currTime := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	nowFn := func() time.Time { return currTime }

	mgr := NewCircuitManager(nowFn, nil)
	key := FormatCircuitKey("model-a", "https://api.example.com")
	policy := CircuitPolicy{
		FailureThreshold: 2,
		OpenDuration:     5 * time.Minute,
		MaxOpenDuration:  15 * time.Minute,
	}
	mgr.Register("model-a", "https://api.example.com", "digest", policy)

	mgr.RecordFailure(t.Context(), key, ErrorClassTransient)
	mgr.RecordFailure(t.Context(), key, ErrorClassTransient)

	currTime = currTime.Add(6 * time.Minute)
	allowed, isProbe := mgr.Allow(key)
	if !allowed || !isProbe {
		t.Fatalf("Allow() = (%v, %v); want (true, true)", allowed, isProbe)
	}

	mgr.RecordFailure(t.Context(), key, ErrorClassTransient)

	currTime = currTime.Add(8 * time.Minute)
	allowed, _ = mgr.Allow(key)
	if allowed {
		t.Fatalf("circuit should remain open during 10m backoff")
	}

	currTime = currTime.Add(3 * time.Minute)
	allowed, isProbe = mgr.Allow(key)
	if !allowed || !isProbe {
		t.Fatalf("Allow() at 11m = (%v, %v); want (true, true)", allowed, isProbe)
	}
}

func TestCircuitManager_AuthFailureLocksOpenUntilReload(t *testing.T) {
	currTime := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	nowFn := func() time.Time { return currTime }

	mgr := NewCircuitManager(nowFn, nil)
	key := FormatCircuitKey("model-auth", "")
	mgr.Register("model-auth", "", "digest", DefaultCircuitPolicy())

	mgr.RecordFailure(t.Context(), key, ErrorClassAuth)

	allowed, _ := mgr.Allow(key)
	if allowed {
		t.Fatalf("Allow() after auth failure should be false")
	}

	currTime = currTime.Add(30 * 24 * time.Hour)
	allowed, _ = mgr.Allow(key)
	if allowed {
		t.Fatalf("Allow() after auth failure should remain false indefinitely")
	}

	if !mgr.Reset(t.Context(), "model-auth") {
		t.Fatalf("Reset failed")
	}

	allowed, _ = mgr.Allow(key)
	if !allowed {
		t.Fatalf("Allow() after reset should be true")
	}
}

func TestCircuitManager_CheckpointSurvivalAndConfigChange(t *testing.T) {
	currTime := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	nowFn := func() time.Time { return currTime }

	store := newMemoryCheckpointStore()
	mgr1 := NewCircuitManager(nowFn, store)
	key := FormatCircuitKey("primary", "https://api.openai.com")

	policy := CircuitPolicy{FailureThreshold: 2, OpenDuration: 5 * time.Minute, MaxOpenDuration: time.Hour}
	mgr1.Register("primary", "https://api.openai.com", "digest-v1", policy)

	mgr1.RecordFailure(t.Context(), key, ErrorClassTransient)
	mgr1.RecordFailure(t.Context(), key, ErrorClassTransient)

	if len(store.checkpoints) != 1 {
		t.Fatalf("expected 1 checkpoint in store, got %d", len(store.checkpoints))
	}

	mgr2 := NewCircuitManager(nowFn, store)
	mgr2.Register("primary", "https://api.openai.com", "digest-v1", policy)
	if err := mgr2.LoadCheckpoints(context.Background()); err != nil {
		t.Fatalf("LoadCheckpoints: %v", err)
	}

	allowed, _ := mgr2.Allow(key)
	if allowed {
		t.Fatalf("restored circuit should be open")
	}

	mgr3 := NewCircuitManager(nowFn, store)
	mgr3.Register("primary", "https://api.openai.com", "digest-v2", policy)
	if err := mgr3.LoadCheckpoints(context.Background()); err != nil {
		t.Fatalf("LoadCheckpoints: %v", err)
	}

	allowed, _ = mgr3.Allow(key)
	if !allowed {
		t.Fatalf("circuit with changed config digest should ignore checkpoint and allow calls")
	}
}

func TestCircuitManager_ConcurrencyRaceFree(t *testing.T) {
	currTime := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	mgr := NewCircuitManager(func() time.Time { return currTime }, nil)
	key := FormatCircuitKey("primary", "")
	mgr.Register("primary", "", "digest", DefaultCircuitPolicy())

	var wg sync.WaitGroup
	workers := 50
	iterations := 20

	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range iterations {
				_, _ = mgr.Allow(key)
				mgr.RecordFailure(context.Background(), key, ErrorClassTransient)
				mgr.RecordSuccess(context.Background(), key)
				_ = mgr.Inspect()
			}
		}()
	}
	wg.Wait()
}

func TestComputeConfigDigest(t *testing.T) {
	def1 := config.ModelDefinition{
		Protocol: config.ProtocolOpenAIChatCompat,
		Model:    "gpt-4o",
		BaseURL:  "https://api.openai.com",
		Capabilities: config.ModelCapabilities{
			ContextTokens: 128000,
			Tokenizer:     "o200k",
		},
	}
	def2 := def1
	def2.Model = "gpt-4o-mini"

	d1 := ComputeConfigDigest(&def1)
	d2 := ComputeConfigDigest(&def2)
	if d1 == "" || d2 == "" {
		t.Fatalf("empty config digest")
	}
	if d1 == d2 {
		t.Fatalf("expected different digests for different models, got same: %s", d1)
	}
}
