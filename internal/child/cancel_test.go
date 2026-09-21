package child

import (
	stdcontext "context"
	"sync"
	"testing"
	"time"
)

type fakeCancelRegistry struct {
	mu     sync.Mutex
	spawns map[string]Spawn
	states map[string]string
	set    [][3]string
}

func (f *fakeCancelRegistry) Get(_ stdcontext.Context, id string) (Spawn, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	spawn, ok := f.spawns[id]
	return spawn, ok, nil
}

func (f *fakeCancelRegistry) RunState(_ stdcontext.Context, id string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.states[id], nil
}

func (f *fakeCancelRegistry) SetChildState(_ stdcontext.Context, id, state string, now time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.set = append(f.set, [3]string{id, state, now.Format(time.RFC3339Nano)})
	f.states[id] = state
	return nil
}

type fakeRuns struct {
	mu        sync.Mutex
	cancelled []string
}

func (f *fakeRuns) CancelRun(_ stdcontext.Context, durableKey string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancelled = append(f.cancelled, durableKey)
	return nil
}

func testCancelService(t *testing.T) (*CancelService, *fakeCancelRegistry, *fakeRuns) {
	t.Helper()
	runs := &fakeRuns{}
	registry := &fakeCancelRegistry{
		spawns: map[string]Spawn{"ch-1": {ID: "ch-1", SessionID: "sess-child", DurableKey: "child/ch-1"}},
		states: map[string]string{"ch-1": StatusRunning},
	}
	service, err := NewCancelService(registry, runs)
	if err != nil {
		t.Fatalf("NewCancelService: %v", err)
	}
	return service, registry, runs
}

func TestCancelRunningChild(t *testing.T) {
	service, registry, runs := testCancelService(t)
	now := time.Now().UTC()
	state, err := service.Cancel(t.Context(), "ch-1", now)
	if err != nil || state != StatusCancelled {
		t.Fatalf("Cancel: %s, %v", state, err)
	}
	if len(runs.cancelled) != 1 || runs.cancelled[0] != "child/ch-1" {
		t.Fatalf("cancelled = %v", runs.cancelled)
	}
	if registry.states["ch-1"] != StatusCancelled {
		t.Fatalf("state = %s", registry.states["ch-1"])
	}
}

func TestCancelTerminalChildIsIdempotent(t *testing.T) {
	service, registry, runs := testCancelService(t)
	now := time.Now().UTC()
	for _, terminal := range []string{StatusSucceeded, StatusFailed, StatusCancelled, StatusDeadline, StatusInterrupted} {
		registry.mu.Lock()
		registry.states["ch-1"] = terminal
		registry.set = nil
		runs.cancelled = nil
		registry.mu.Unlock()
		state, err := service.Cancel(t.Context(), "ch-1", now)
		if err != nil || state != terminal {
			t.Fatalf("Cancel %s: %s, %v", terminal, state, err)
		}
		if len(runs.cancelled) != 0 || len(registry.set) != 0 {
			t.Fatalf("terminal cancel must not touch runs: %v %v", runs.cancelled, registry.set)
		}
	}
}

func TestCancelMissingChild(t *testing.T) {
	service, _, _ := testCancelService(t)
	if _, err := service.Cancel(t.Context(), "missing", time.Now().UTC()); err == nil {
		t.Fatal("expected not-found")
	} else if code, ok := CodeOf(err); !ok || code != ErrorCodeChildNotFound {
		t.Fatalf("code = %v, %v (%v)", code, ok, err)
	}
	var nilCtx stdcontext.Context
	if _, err := service.Cancel(nilCtx, "ch-1", time.Now().UTC()); err == nil {
		t.Error("expected nil context rejection")
	}
	if _, err := NewCancelService(nil, &fakeRuns{}); err == nil {
		t.Error("expected nil registry rejection")
	}
	if _, err := NewCancelService(&fakeCancelRegistry{}, nil); err == nil {
		t.Error("expected nil runs rejection")
	}
}

func TestCancelValidatesTimestamps(t *testing.T) {
	service, _, _ := testCancelService(t)
	if _, err := service.Cancel(t.Context(), "ch-1", time.Time{}); err == nil {
		t.Error("expected zero time rejection")
	}
}
