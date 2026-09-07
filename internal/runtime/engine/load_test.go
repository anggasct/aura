//go:build load

package runtimeengine

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"testing/synctest"

	auraruntime "github.com/anggasct/aura/internal/runtime"
)

func TestLoadBoundsUnderVolume(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		gate := make(chan struct{})
		executor := auraruntime.NewFakeExecutor([]auraruntime.FakeStep{{Kind: auraruntime.EventKindModelStarted, Block: gate, Payload: json.RawMessage(`{}`)}})
		cfg := Config{MaxActiveTurns: 4, MaxPendingTurns: 8}
		engine, db, _ := newTestRuntime(t, cfg, executor)

		const n = 50
		for i := range n {
			mustCreateSession(t, db, fmt.Sprintf("session-%d", i))
		}

		overflow := make(chan error, n)
		var wg sync.WaitGroup
		for i := range n {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				req := sampleRequest(fmt.Sprintf("session-%d", i), fmt.Sprintf("turn-%d", i))
				sub, err := engine.submit(context.Background(), req)
				if err != nil {
					overflow <- err
					return
				}
				sub.stop() // abandon the stream; the turn still runs to a durable terminal
			}(i)
		}
		wg.Wait()
		close(overflow)

		var overflowCount int
		for err := range overflow {
			if code, ok := auraruntime.CodeOf(err); !ok || code != auraruntime.ErrorCodeRuntimeOverloaded {
				t.Errorf("overflow error code = %q (ok=%v), want runtime_overloaded", code, ok)
			}
			overflowCount++
		}
		accepted := n - overflowCount
		if accepted == 0 {
			t.Fatal("no turns were accepted under volume")
		}

		counts := func() (int, int) {
			engine.mu.Lock()
			defer engine.mu.Unlock()
			return engine.active, engine.pending
		}
		wantActive := min(accepted, cfg.MaxActiveTurns)
		waitFor(t, func() bool { return executor.StartCount() == wantActive })
		active, pending := counts()
		if active > cfg.MaxActiveTurns {
			t.Errorf("active = %d, exceeds max %d", active, cfg.MaxActiveTurns)
		}
		if pending > cfg.MaxPendingTurns {
			t.Errorf("pending = %d, exceeds max %d", pending, cfg.MaxPendingTurns)
		}
		if active+pending != accepted {
			t.Errorf("active+pending = %d, want accepted %d", active+pending, accepted)
		}

		close(gate)
		waitFor(t, func() bool { return executor.StartCount() == accepted })
	})
}

func TestLoadNoLeakAfterAbandonedConsumers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		executor := auraruntime.NewFakeExecutor([]auraruntime.FakeStep{jsonStep(auraruntime.EventKindModelStarted), jsonStep(auraruntime.EventKindMessageCompleted)})
		cfg := Config{MaxActiveTurns: 4, MaxPendingTurns: 64}
		engine, db, _ := newTestRuntime(t, cfg, executor)

		const n = 30
		for i := range n {
			mustCreateSession(t, db, fmt.Sprintf("session-%d", i))
		}

		baseline := runtime.NumGoroutine()
		var wg sync.WaitGroup
		for i := range n {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				req := sampleRequest(fmt.Sprintf("session-%d", i), fmt.Sprintf("turn-%d", i))
				for _, err := range engine.Run(context.Background(), req) {
					_ = err
					break // abandon the stream; the turn must still complete durably
				}
			}(i)
		}
		wg.Wait()

		waitFor(t, func() bool { return executor.StartCount() == n })
		waitFor(t, func() bool { return runtime.NumGoroutine() <= baseline })
	})
}
