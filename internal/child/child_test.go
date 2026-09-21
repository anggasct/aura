package child

import (
	stdcontext "context"
	"errors"
	"sync"
	"testing"
	"time"
)

type fakeRegistry struct {
	mu      sync.Mutex
	spawns  map[string]Spawn
	byKey   map[string]string
	spawned int
	fail    error
}

func newFakeRegistry() *fakeRegistry {
	return &fakeRegistry{spawns: map[string]Spawn{}, byKey: map[string]string{}}
}

func (f *fakeRegistry) Spawn(_ stdcontext.Context, spec *Spec, now time.Time) (Spawn, bool, error) {
	if f.fail != nil {
		return Spawn{}, false, f.fail
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	key := spec.ParentInvocation + "\x00" + spec.IdempotencyKey
	if id, ok := f.byKey[key]; ok {
		existing := f.spawns[id]
		if existing.ContextDigest != spec.ContextDigest {
			return Spawn{}, false, Errorf(ErrorCodeChildConflict, "child run conflicts")
		}
		return existing, false, nil
	}
	f.spawned++
	spawn := Spawn{
		ID: spec.ID, SessionID: spec.ChildSessionID, Depth: spec.ParentDepth + 1,
		Grants: spec.RequestedGrants, DurableKey: DurableChildKey(spec.ID),
		ContextDigest: spec.ContextDigest, Deadline: now.Add(spec.Budget.Timeout), CreatedAt: now,
	}
	f.spawns[spec.ID] = spawn
	f.byKey[key] = spec.ID
	return spawn, true, nil
}

func (f *fakeRegistry) Get(_ stdcontext.Context, id string) (Spawn, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	spawn, ok := f.spawns[id]
	return spawn, ok, nil
}

func testSpec() *Spec {
	return &Spec{
		ID: "ch-1", IdempotencyKey: "key-1",
		ParentSessionID: "sess-parent", ParentTurnID: "turn-1", ParentInvocation: "inv-1",
		ChildSessionID: "sess-child", ParentDepth: 0,
		ParentGrants:    []Grant{{Capability: "search"}, {Capability: "read"}},
		Task:            "summarize the logs",
		References:      []Reference{{Kind: "event", ID: "evt-1"}},
		RequestedGrants: []Grant{{Capability: "search"}},
		Provider:        "test", Model: "m1",
		Budget: Budget{MaxTokens: 1000, MaxCost: 10, MaxTools: 4, Timeout: time.Minute},
	}
}

func TestSpawnChildRoundTrip(t *testing.T) {
	service, err := NewService(newFakeRegistry())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	now := time.Now().UTC()
	spawned, created, err := service.SpawnChild(t.Context(), testSpec(), now)
	if err != nil || !created {
		t.Fatalf("SpawnChild: %+v, %v, %v", spawned, created, err)
	}
	if spawned.Depth != 1 || spawned.DurableKey != "child/ch-1" {
		t.Errorf("spawn = %+v", spawned)
	}
	if len(spawned.Grants) != 1 || spawned.Grants[0].Capability != "search" {
		t.Errorf("grants = %+v", spawned.Grants)
	}
	again, created, err := service.SpawnChild(t.Context(), testSpec(), now)
	if err != nil || created || again.ID != "ch-1" {
		t.Fatalf("idempotent replay: %+v, %v, %v", again, created, err)
	}
	altered := testSpec()
	altered.Task = "different work"
	if _, _, err := service.SpawnChild(t.Context(), altered, now); err == nil {
		t.Fatal("altered replay must conflict")
	} else if code, ok := CodeOf(err); !ok || code != ErrorCodeChildConflict {
		t.Fatalf("code = %v, %v (%v)", code, ok, err)
	}
}

func TestSpawnChildIgnoresCallerDigest(t *testing.T) {
	service, err := NewService(newFakeRegistry())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	now := time.Now().UTC()
	first := testSpec()
	first.ContextDigest = "forged-digest"
	spawned, created, err := service.SpawnChild(t.Context(), first, now)
	if err != nil || !created {
		t.Fatalf("SpawnChild: %+v, %v, %v", spawned, created, err)
	}
	if spawned.ContextDigest == "forged-digest" {
		t.Fatal("caller digest must be recomputed")
	}
	if want := ContextDigest(first.Task, first.References); spawned.ContextDigest != want {
		t.Fatalf("digest = %q, want %q", spawned.ContextDigest, want)
	}
	altered := testSpec()
	altered.Task = "different work"
	altered.ContextDigest = "forged-digest"
	if _, _, err := service.SpawnChild(t.Context(), altered, now); err == nil {
		t.Fatal("altered task with shared explicit digest must conflict")
	} else if code, ok := CodeOf(err); !ok || code != ErrorCodeChildConflict {
		t.Fatalf("code = %v, %v (%v)", code, ok, err)
	}
	same := testSpec()
	same.ContextDigest = "another-forged-value"
	replay, created, err := service.SpawnChild(t.Context(), same, now)
	if err != nil || created || replay.ID != "ch-1" {
		t.Fatalf("identical task with different caller digest must replay: %+v, %v, %v", replay, created, err)
	}
	if replay.ContextDigest != spawned.ContextDigest {
		t.Fatalf("replay digest = %q, want %q", replay.ContextDigest, spawned.ContextDigest)
	}
}

func TestSpawnChildReplayReturnsPersistedGrants(t *testing.T) {
	registry := newFakeRegistry()
	service, err := NewService(registry)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	now := time.Now().UTC()
	first := testSpec()
	first.RequestedGrants = []Grant{{Capability: "read"}, {Capability: "search"}}
	spawned, created, err := service.SpawnChild(t.Context(), first, now)
	if err != nil || !created {
		t.Fatalf("SpawnChild: %+v, %v, %v", spawned, created, err)
	}
	replaySpec := testSpec()
	replaySpec.RequestedGrants = []Grant{{Capability: "search"}}
	replay, created, err := service.SpawnChild(t.Context(), replaySpec, now)
	if err != nil || created {
		t.Fatalf("replay: %+v, %v, %v", replay, created, err)
	}
	if len(replay.Grants) != len(spawned.Grants) {
		t.Fatalf("replay grants = %+v, want %+v", replay.Grants, spawned.Grants)
	}
	for i := range spawned.Grants {
		if replay.Grants[i] != spawned.Grants[i] {
			t.Fatalf("replay grants = %+v, want %+v", replay.Grants, spawned.Grants)
		}
	}
	stored, found, err := registry.Get(t.Context(), spawned.ID)
	if err != nil || !found {
		t.Fatalf("Get: %+v, %v, %v", stored, found, err)
	}
	if len(stored.Grants) != len(spawned.Grants) {
		t.Fatalf("stored grants = %+v, want %+v", stored.Grants, spawned.Grants)
	}
	for i := range spawned.Grants {
		if stored.Grants[i] != spawned.Grants[i] {
			t.Fatalf("stored grants = %+v, want %+v", stored.Grants, spawned.Grants)
		}
	}
}

func TestSpawnChildValidation(t *testing.T) {
	service, err := NewService(newFakeRegistry())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	now := time.Now().UTC()
	for name, mutate := range map[string]func(*Spec){
		"depth":    func(spec *Spec) { spec.ParentDepth = 1 },
		"spawn":    func(spec *Spec) { spec.RequestedGrants = []Grant{{Capability: "spawn_child"}} },
		"widen":    func(spec *Spec) { spec.RequestedGrants = []Grant{{Capability: "write"}} },
		"empty":    func(spec *Spec) { spec.Task = "" },
		"timeout":  func(spec *Spec) { spec.Budget.Timeout = 0 },
		"negative": func(spec *Spec) { spec.Budget.MaxTokens = -1 },
		"ref":      func(spec *Spec) { spec.References = []Reference{{Kind: "", ID: ""}} },
	} {
		spec := testSpec()
		mutate(spec)
		if _, _, err := service.SpawnChild(t.Context(), spec, now); err == nil {
			t.Errorf("%s: expected rejection", name)
		}
	}
	var nilCtx stdcontext.Context
	if _, _, err := service.SpawnChild(nilCtx, testSpec(), now); err == nil {
		t.Error("expected nil context rejection")
	}
	if _, _, err := service.SpawnChild(t.Context(), nil, now); err == nil {
		t.Error("expected nil spec rejection")
	}
	cancelled, cancel := stdcontext.WithCancel(stdcontext.Background())
	cancel()
	if _, _, err := service.SpawnChild(cancelled, testSpec(), now); !errors.Is(err, stdcontext.Canceled) {
		t.Errorf("cancelled context: %v", err)
	}
	register := newFakeRegistry()
	register.fail = errors.New("storage down")
	broken, err := NewService(register)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	if _, _, err := broken.SpawnChild(t.Context(), testSpec(), now); err == nil {
		t.Error("expected registry failure propagation")
	}
	if _, err := NewService(nil); err == nil {
		t.Error("expected nil registry rejection")
	}
}

func TestAttenuateGrants(t *testing.T) {
	parent := []Grant{{Capability: "Search"}, {Capability: "read"}}
	child, err := attenuateGrants(parent, []Grant{{Capability: "search"}, {Capability: "READ", Scope: "docs"}})
	if err != nil {
		t.Fatalf("attenuateGrants: %v", err)
	}
	if len(child) != 2 || child[0].Capability != "READ" || child[1].Capability != "search" {
		t.Errorf("child = %+v", child)
	}
	if _, err := attenuateGrants(parent, []Grant{{Capability: "write"}}); err == nil {
		t.Error("expected widening rejection")
	}
	if _, err := attenuateGrants(nil, []Grant{{Capability: "read"}}); err == nil {
		t.Error("expected empty-parent rejection")
	}
}

func TestContextDigestStable(t *testing.T) {
	spec := testSpec()
	if ContextDigest(spec.Task, spec.References) == "" {
		t.Fatal("digest must not be empty")
	}
	if first, second := ContextDigest("a", nil), ContextDigest("a", nil); first != second {
		t.Fatal("digest must be deterministic")
	}
	if ContextDigest("a", nil) == ContextDigest("b", nil) {
		t.Fatal("digest must cover the task")
	}
}
