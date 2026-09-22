package child

import (
	stdcontext "context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/durable"
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

func (f *fakeCancelRegistry) ActiveForParent(_ stdcontext.Context, parentSessionID string) ([]Spawn, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Spawn
	for id := range f.spawns {
		spawn := f.spawns[id]
		if spawn.ParentSessionID != "" && spawn.ParentSessionID != parentSessionID {
			continue
		}
		if spawn.ParentSessionID == "" && parentSessionID != "sess-parent" {
			continue
		}
		state := f.states[spawn.ID]
		switch state {
		case StatusSucceeded, StatusFailed, StatusCancelled, StatusDeadline, StatusInterrupted:
			continue
		}
		out = append(out, spawn)
	}
	return out, nil
}

type fakeRuns struct {
	mu        sync.Mutex
	cancelled []string
	fail      error
}

func (f *fakeRuns) CancelRun(_ stdcontext.Context, durableKey string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancelled = append(f.cancelled, durableKey)
	return f.fail
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

func TestCancelDurableFailureLeavesStateUnchanged(t *testing.T) {
	service, registry, runs := testCancelService(t)
	runs.fail = errors.New("durable down")
	if _, err := service.Cancel(t.Context(), "ch-1", time.Now().UTC()); err == nil {
		t.Fatal("expected durable failure")
	}
	if len(runs.cancelled) != 1 || runs.cancelled[0] != "child/ch-1" {
		t.Fatalf("durable must be called with child key, cancelled = %v", runs.cancelled)
	}
	if registry.states["ch-1"] != StatusRunning {
		t.Fatalf("durable failure must leave state unchanged, state = %s", registry.states["ch-1"])
	}
	if len(registry.set) != 0 {
		t.Fatalf("durable failure must not touch projection, set = %v", registry.set)
	}
}

func TestCancelUnknownDurableIsIdempotent(t *testing.T) {
	service, registry, runs := testCancelService(t)
	runs.fail = durable.ErrUnknownRun
	state, err := service.Cancel(t.Context(), "ch-1", time.Now().UTC())
	if err != nil || state != StatusCancelled {
		t.Fatalf("unknown durable must settle as cancelled: %s, %v", state, err)
	}
	if registry.states["ch-1"] != StatusCancelled {
		t.Fatalf("state = %s", registry.states["ch-1"])
	}
}

func testTreeService(t *testing.T) (*CancelService, *fakeCancelRegistry, *fakeRuns) {
	t.Helper()
	runs := &fakeRuns{}
	registry := &fakeCancelRegistry{
		spawns: map[string]Spawn{
			"ch-1": {ID: "ch-1", SessionID: "sess-1", DurableKey: "child/ch-1", ParentSessionID: "sess-parent"},
			"ch-2": {ID: "ch-2", SessionID: "sess-2", DurableKey: "child/ch-2", ParentSessionID: "sess-parent"},
			"ch-3": {ID: "ch-3", SessionID: "sess-3", DurableKey: "child/ch-3", ParentSessionID: "sess-parent"},
		},
		states: map[string]string{"ch-1": StatusRunning, "ch-2": StatusQueued, "ch-3": StatusSucceeded},
	}
	service, err := NewCancelService(registry, runs)
	if err != nil {
		t.Fatalf("NewCancelService: %v", err)
	}
	return service, registry, runs
}

func TestCancelTreeCancelsNonterminalChildren(t *testing.T) {
	service, registry, runs := testTreeService(t)
	results, err := service.CancelTree(t.Context(), "sess-parent", time.Now().UTC(), false)
	if err != nil {
		t.Fatalf("CancelTree: %v", err)
	}
	if len(results) != 2 || results["ch-1"] != StatusCancelled || results["ch-2"] != StatusCancelled {
		t.Fatalf("results = %v", results)
	}
	if registry.states["ch-1"] != StatusCancelled || registry.states["ch-2"] != StatusCancelled {
		t.Fatalf("states = %v", registry.states)
	}
	if registry.states["ch-3"] != StatusSucceeded {
		t.Fatalf("terminal must survive, states = %v", registry.states)
	}
	if len(runs.cancelled) != 2 {
		t.Fatalf("durable calls = %v", runs.cancelled)
	}
}

func TestCancelTreeLeavesBackgroundRunning(t *testing.T) {
	runs := &fakeRuns{}
	registry := &fakeCancelRegistry{
		spawns: map[string]Spawn{
			"ch-1":  {ID: "ch-1", SessionID: "sess-1", DurableKey: "child/ch-1", ParentSessionID: "sess-parent"},
			"ch-bg": {ID: "ch-bg", SessionID: "sess-bg", DurableKey: "child/ch-bg", ParentSessionID: "sess-parent", Background: true},
		},
		states: map[string]string{"ch-1": StatusRunning, "ch-bg": StatusRunning},
	}
	service, err := NewCancelService(registry, runs)
	if err != nil {
		t.Fatalf("NewCancelService: %v", err)
	}
	results, err := service.CancelTree(t.Context(), "sess-parent", time.Now().UTC(), true)
	if err != nil {
		t.Fatalf("CancelTree: %v", err)
	}
	if results["ch-1"] != StatusCancelled || results["ch-bg"] != StatusRunning {
		t.Fatalf("background must survive, results = %v", results)
	}
	if registry.states["ch-bg"] != StatusRunning {
		t.Fatalf("background state = %s", registry.states["ch-bg"])
	}
	state, err := service.Cancel(t.Context(), "ch-bg", time.Now().UTC())
	if err != nil || state != StatusCancelled {
		t.Fatalf("background cancel: %s, %v", state, err)
	}
}

func TestCancelTreeDeadlineSettlesDeadline(t *testing.T) {
	runs := &fakeRuns{}
	past := time.Now().UTC().Add(-time.Minute)
	registry := &fakeCancelRegistry{
		spawns: map[string]Spawn{
			"ch-1": {ID: "ch-1", SessionID: "sess-1", DurableKey: "child/ch-1", ParentSessionID: "sess-parent", Deadline: past},
		},
		states: map[string]string{"ch-1": StatusRunning},
	}
	service, err := NewCancelService(registry, runs)
	if err != nil {
		t.Fatalf("NewCancelService: %v", err)
	}
	state, err := service.Cancel(t.Context(), "ch-1", time.Now().UTC())
	if err != nil || state != StatusDeadline {
		t.Fatalf("past deadline must settle deadline: %s, %v", state, err)
	}
	if registry.states["ch-1"] != StatusDeadline {
		t.Fatalf("state = %s", registry.states["ch-1"])
	}
}

func TestCancelTreeOverloadIsDeterministic(t *testing.T) {
	runs := &fakeRuns{}
	spawns := map[string]Spawn{}
	states := map[string]string{}
	for i := range 6 {
		id := "ch-" + string(rune('0'+i))
		spawns[id] = Spawn{ID: id, SessionID: "sess-" + id, DurableKey: "child/" + id, ParentSessionID: "sess-parent"}
		states[id] = StatusRunning
	}
	registry := &fakeCancelRegistry{spawns: spawns, states: states}
	service, err := NewCancelService(registry, runs)
	if err != nil {
		t.Fatalf("NewCancelService: %v", err)
	}
	if _, err := service.CancelTree(t.Context(), "sess-parent", time.Now().UTC(), false); err == nil {
		t.Fatal("overload must fail")
	} else if code, ok := CodeOf(err); !ok || code != ErrorCodeRuntimeOverloaded {
		t.Fatalf("code = %v, %v (%v)", code, ok, err)
	}
	if len(runs.cancelled) != 0 {
		t.Fatalf("overload must not touch runtime, cancelled = %v", runs.cancelled)
	}
}

func TestCancelTreeShutdownDrainsWithoutLeaks(t *testing.T) {
	service, registry, runs := testTreeService(t)
	before := len(runs.cancelled)
	cancelled, cancel := stdcontext.WithCancel(t.Context())
	cancel()
	results, err := service.CancelTree(cancelled, "sess-parent", time.Now().UTC(), false)
	if err == nil {
		t.Fatal("shutdown must report interruption")
	} else if code, ok := CodeOf(err); !ok || code != ErrorCodeTurnCancelled {
		t.Fatalf("code = %v, %v (%v)", code, ok, err)
	}
	if len(results) != 2 {
		t.Fatalf("shutdown must still drain, results = %v", results)
	}
	if registry.states["ch-1"] != StatusCancelled || registry.states["ch-2"] != StatusCancelled {
		t.Fatalf("drain must persist terminal states, states = %v", registry.states)
	}
	if len(runs.cancelled) != before+2 {
		t.Fatalf("drain must cancel runtime, cancelled = %v", runs.cancelled)
	}
}

func TestCancelTreeSlowConsumerSettles(t *testing.T) {
	runs := &fakeRuns{}
	registry := &slowCancelRegistry{
		fakeCancelRegistry: &fakeCancelRegistry{
			spawns: map[string]Spawn{
				"ch-1": {ID: "ch-1", SessionID: "sess-1", DurableKey: "child/ch-1", ParentSessionID: "sess-parent"},
				"ch-2": {ID: "ch-2", SessionID: "sess-2", DurableKey: "child/ch-2", ParentSessionID: "sess-parent"},
			},
			states: map[string]string{"ch-1": StatusRunning, "ch-2": StatusRunning},
		},
		delay: 50 * time.Millisecond,
	}
	service, err := NewCancelService(registry, runs)
	if err != nil {
		t.Fatalf("NewCancelService: %v", err)
	}
	done := make(chan map[string]string, 1)
	go func() {
		results, err := service.CancelTree(t.Context(), "sess-parent", time.Now().UTC(), false)
		if err != nil {
			t.Errorf("CancelTree slow: %v", err)
			done <- nil
			return
		}
		done <- results
	}()
	select {
	case results := <-done:
		if len(results) != 2 {
			t.Fatalf("slow results = %v", results)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("slow consumer must still settle")
	}
}

type slowCancelRegistry struct {
	*fakeCancelRegistry
	delay time.Duration
}

func (s *slowCancelRegistry) SetChildState(ctx stdcontext.Context, id, state string, now time.Time) error {
	select {
	case <-time.After(s.delay):
	case <-ctx.Done():
		return ctx.Err()
	}
	return s.fakeCancelRegistry.SetChildState(ctx, id, state, now)
}
