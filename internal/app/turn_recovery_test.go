package app

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

const testTurnRecoveryInterval = time.Minute

type scriptedRecoverer struct {
	mu      sync.Mutex
	results []error
	calls   int
}

func (r *scriptedRecoverer) RecoverTurns(context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	if r.calls > len(r.results) {
		return nil
	}
	return r.results[r.calls-1]
}

func (r *scriptedRecoverer) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

func newTestTurnRecovery(results ...error) (*turnRecovery, *scriptedRecoverer, *syncBuffer) {
	recoverer := &scriptedRecoverer{results: results}
	logs := &syncBuffer{}
	return &turnRecovery{
		recoverer: recoverer,
		logger:    slog.New(slog.NewJSONHandler(logs, nil)),
		interval:  testTurnRecoveryInterval,
	}, recoverer, logs
}

func runTurnRecoveryPasses(t *testing.T, recovery *turnRecovery, passes int) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		recovery.run(ctx)
		close(done)
	}()
	time.Sleep(time.Duration(passes)*recovery.interval + recovery.interval/2)
	synctest.Wait()
	cancel()
	<-done
}

func requireRecoveryWarnings(t *testing.T, logs *syncBuffer, want int) {
	t.Helper()
	warnings := recordsAt(t, logs, "WARN")
	if len(warnings) != want {
		t.Fatalf("warnings = %+v, want %d", warnings, want)
	}
	for _, warning := range warnings {
		if warning.Msg != "turn recovery failed" {
			t.Fatalf("warning = %+v, want a failed turn recovery", warning)
		}
	}
}

func TestTurnRecoveryPassRepeats(t *testing.T) {
	t.Run("a failing recovery at the start is logged once and does not fail the start", func(t *testing.T) {
		recovery, recoverer, logs := newTestTurnRecovery(errors.New("database is locked"))
		if err := recovery.start(t.Context()); err != nil {
			t.Fatalf("start = %v, want nil", err)
		}
		if got := recoverer.count(); got != 1 {
			t.Fatalf("recoveries at the start = %d, want 1", got)
		}
		requireRecoveryWarnings(t, logs, 1)
	})

	t.Run("the pass recovers once per interval", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			recovery, recoverer, logs := newTestTurnRecovery()
			runTurnRecoveryPasses(t, recovery, 3)
			if got := recoverer.count(); got != 3 {
				t.Fatalf("recoveries after three intervals = %d, want 3", got)
			}
			requireRecoveryWarnings(t, logs, 0)
		})
	})

	t.Run("a failing pass is logged once and the next pass runs", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			recovery, recoverer, logs := newTestTurnRecovery(errors.New("database is locked"))
			runTurnRecoveryPasses(t, recovery, 2)
			if got := recoverer.count(); got != 2 {
				t.Fatalf("recoveries after two intervals = %d, want 2", got)
			}
			requireRecoveryWarnings(t, logs, 1)
		})
	})

	t.Run("nothing recovers under an ended context", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			recovery, recoverer, logs := newTestTurnRecovery(errors.New("database is locked"))
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			if err := recovery.start(ctx); err != nil {
				t.Fatalf("start under an ended context = %v, want nil", err)
			}
			recovery.run(ctx)
			if got := recoverer.count(); got != 0 {
				t.Fatalf("recoveries under an ended context = %d, want 0", got)
			}
			if got := logs.String(); got != "" {
				t.Fatalf("logs under an ended context = %q, want none", got)
			}
		})
	})
}
