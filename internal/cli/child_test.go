package cli

import (
	"sync"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/child"
	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/store"
)

func TestChildRegistrySpawnIdempotent(t *testing.T) {
	cfgPath := writeProfileCLIConfig(t)
	loaded, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	db, err := openStorage(t.Context(), loaded.Config)
	if err != nil {
		t.Fatalf("openStorage: %v", err)
	}
	defer func() { _ = db.Close() }()
	now := time.Now().UTC()
	sessions := store.NewSessionService(db)
	for _, id := range []string{"sess-parent", "sess-child-1"} {
		if err := sessions.Create(t.Context(), &store.Session{ID: id, OwnerID: "owner-1", CreatedAt: now, UpdatedAt: now, Metadata: []byte(`{}`)}); err != nil {
			t.Fatalf("Create session %s: %v", id, err)
		}
	}
	service, err := child.NewService(newChildRegistry(db))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	spec := &child.Spec{
		ID: "ch-1", IdempotencyKey: "key-1",
		ParentSessionID: "sess-parent", ParentTurnID: "turn-1", ParentInvocation: "inv-1",
		ChildSessionID: "sess-child-1", ParentDepth: 0,
		ParentGrants:    []child.Grant{{Capability: "search"}},
		Task:            "summarize the logs",
		RequestedGrants: []child.Grant{{Capability: "search"}},
		Budget:          child.Budget{MaxTokens: 1000, Timeout: time.Minute},
	}
	first, created, err := service.SpawnChild(t.Context(), spec, now)
	if err != nil || !created {
		t.Fatalf("SpawnChild: %+v, %v, %v", first, created, err)
	}
	if first.DurableKey != "child/ch-1" || first.Depth != 1 {
		t.Errorf("spawn = %+v", first)
	}
	second, created, err := service.SpawnChild(t.Context(), spec, now)
	if err != nil || created || second.ID != "ch-1" {
		t.Fatalf("idempotent replay: %+v, %v, %v", second, created, err)
	}
	if second.Depth != first.Depth {
		t.Fatalf("replay depth = %d, want %d", second.Depth, first.Depth)
	}
	if len(second.Grants) != len(first.Grants) {
		t.Fatalf("replay grants = %+v, want %+v", second.Grants, first.Grants)
	}
	for i := range first.Grants {
		if second.Grants[i] != first.Grants[i] {
			t.Fatalf("replay grants = %+v, want %+v", second.Grants, first.Grants)
		}
	}
	rival := *spec
	rival.Task = "other work"
	if _, _, err := service.SpawnChild(t.Context(), &rival, now); err == nil {
		t.Fatal("altered replay must conflict")
	} else if code, ok := child.CodeOf(err); !ok || code != child.ErrorCodeChildConflict {
		t.Fatalf("code = %v, %v (%v)", code, ok, err)
	}
	deep := *spec
	deep.ID = "ch-2"
	deep.IdempotencyKey = "key-2"
	deep.ChildSessionID = "sess-child-2"
	deep.ParentDepth = 1
	if _, _, err := service.SpawnChild(t.Context(), &deep, now); err == nil {
		t.Fatal("depth above one must fail")
	} else if code, ok := child.CodeOf(err); !ok || code != child.ErrorCodeChildDepthExceeded {
		t.Fatalf("code = %v, %v (%v)", code, ok, err)
	}
	widened := *spec
	widened.ID = "ch-3"
	widened.IdempotencyKey = "key-3"
	widened.ChildSessionID = "sess-child-3"
	widened.RequestedGrants = []child.Grant{{Capability: "write"}}
	if _, _, err := service.SpawnChild(t.Context(), &widened, now); err == nil {
		t.Fatal("widening grants must fail")
	}
	registry, err := child.NewService(newChildRegistry(db))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	if _, _, err := registry.SpawnChild(t.Context(), nil, now); err == nil {
		t.Fatal("nil spec must fail")
	}
}

func openChildTestService(t *testing.T, sessions ...string) (*child.Service, time.Time) {
	t.Helper()
	cfgPath := writeProfileCLIConfig(t)
	loaded, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	db, err := openStorage(t.Context(), loaded.Config)
	if err != nil {
		t.Fatalf("openStorage: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	now := time.Now().UTC()
	storeSessions := store.NewSessionService(db)
	for _, id := range sessions {
		if err := storeSessions.Create(t.Context(), &store.Session{ID: id, OwnerID: "owner-1", CreatedAt: now, UpdatedAt: now, Metadata: []byte(`{}`)}); err != nil {
			t.Fatalf("Create session %s: %v", id, err)
		}
	}
	service, err := child.NewService(newChildRegistry(db))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return service, now
}

func TestChildRegistryConcurrentIdenticalSpawn(t *testing.T) {
	service, now := openChildTestService(t, "sess-parent", "sess-child-conc")
	base := &child.Spec{
		ID: "ch-conc", IdempotencyKey: "key-conc",
		ParentSessionID: "sess-parent", ParentTurnID: "turn-1", ParentInvocation: "inv-conc",
		ChildSessionID: "sess-child-conc", ParentDepth: 0,
		ParentGrants:    []child.Grant{{Capability: "search"}},
		Task:            "concurrent work",
		RequestedGrants: []child.Grant{{Capability: "search"}},
		Budget:          child.Budget{MaxTokens: 1000, Timeout: time.Minute},
	}
	const workers = 8
	type result struct {
		id      string
		created bool
		err     error
	}
	results := make([]result, workers)
	var wg sync.WaitGroup
	for i := range workers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			spec := *base
			spawned, created, err := service.SpawnChild(t.Context(), &spec, now)
			results[i] = result{id: spawned.ID, created: created, err: err}
		}(i)
	}
	wg.Wait()
	createdCount := 0
	for _, r := range results {
		if r.err != nil {
			t.Fatalf("concurrent spawn: %v", r.err)
		}
		if r.id != "ch-conc" {
			t.Fatalf("concurrent spawn id = %q, want %q", r.id, "ch-conc")
		}
		if r.created {
			createdCount++
		}
	}
	if createdCount != 1 {
		t.Fatalf("created count = %d, want 1", createdCount)
	}
}

func TestChildRegistryConcurrentAlteredSpawnConflicts(t *testing.T) {
	sessions := []string{"sess-parent"}
	for i := range 8 {
		sessions = append(sessions, "sess-child-alt-"+string(rune('0'+i)))
	}
	service, now := openChildTestService(t, sessions...)
	const workers = 8
	type result struct {
		err error
	}
	results := make([]result, workers)
	var wg sync.WaitGroup
	for i := range workers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			spec := &child.Spec{
				ID: "ch-alt-" + string(rune('0'+i)), IdempotencyKey: "key-alt",
				ParentSessionID: "sess-parent", ParentTurnID: "turn-1", ParentInvocation: "inv-alt",
				ChildSessionID: "sess-child-alt-" + string(rune('0'+i)), ParentDepth: 0,
				ParentGrants:    []child.Grant{{Capability: "search"}},
				Task:            "task-variant-" + string(rune('0'+i)),
				RequestedGrants: []child.Grant{{Capability: "search"}},
				Budget:          child.Budget{MaxTokens: 1000, Timeout: time.Minute},
			}
			_, _, err := service.SpawnChild(t.Context(), spec, now)
			results[i] = result{err: err}
		}(i)
	}
	wg.Wait()
	succeeded := 0
	conflicted := 0
	for _, r := range results {
		if r.err == nil {
			succeeded++
			continue
		}
		if code, ok := child.CodeOf(r.err); ok && code == child.ErrorCodeChildConflict {
			conflicted++
			continue
		}
		t.Fatalf("unexpected error: %v", r.err)
	}
	if succeeded != 1 || conflicted != workers-1 {
		t.Fatalf("succeeded = %d conflicted = %d, want 1 and %d", succeeded, conflicted, workers-1)
	}
}

func TestChildRegistryDepthEnforcedFromDurableState(t *testing.T) {
	service, now := openChildTestService(t, "sess-top", "sess-mid", "sess-leaf")
	first := &child.Spec{
		ID: "ch-mid", IdempotencyKey: "key-mid",
		ParentSessionID: "sess-top", ParentTurnID: "turn-1", ParentInvocation: "inv-mid",
		ChildSessionID: "sess-mid", ParentDepth: 0,
		ParentGrants:    []child.Grant{{Capability: "search"}},
		Task:            "mid work",
		RequestedGrants: []child.Grant{{Capability: "search"}},
		Budget:          child.Budget{MaxTokens: 1000, Timeout: time.Minute},
	}
	spawned, created, err := service.SpawnChild(t.Context(), first, now)
	if err != nil || !created || spawned.Depth != 1 {
		t.Fatalf("SpawnChild: %+v, %v, %v", spawned, created, err)
	}
	replay, created, err := service.SpawnChild(t.Context(), first, now)
	if err != nil || created || replay.Depth != spawned.Depth {
		t.Fatalf("replay depth: %+v, %v, %v", replay, created, err)
	}
	leaf := &child.Spec{
		ID: "ch-leaf", IdempotencyKey: "key-leaf",
		ParentSessionID: "sess-mid", ParentTurnID: "turn-2", ParentInvocation: "inv-leaf",
		ChildSessionID: "sess-leaf", ParentDepth: 0,
		ParentGrants:    []child.Grant{{Capability: "search"}},
		Task:            "leaf work",
		RequestedGrants: []child.Grant{{Capability: "search"}},
		Budget:          child.Budget{MaxTokens: 1000, Timeout: time.Minute},
	}
	if _, _, err := service.SpawnChild(t.Context(), leaf, now); err == nil {
		t.Fatal("grandchild spawn must fail")
	} else if code, ok := child.CodeOf(err); !ok || code != child.ErrorCodeChildDepthExceeded {
		t.Fatalf("code = %v, %v (%v)", code, ok, err)
	}
}

func TestChildRegistryGrantsCanonicalAcrossReplay(t *testing.T) {
	service, now := openChildTestService(t, "sess-parent", "sess-child-1")
	spec := &child.Spec{
		ID: "ch-1", IdempotencyKey: "key-1",
		ParentSessionID: "sess-parent", ParentTurnID: "turn-1", ParentInvocation: "inv-1",
		ChildSessionID: "sess-child-1", ParentDepth: 0,
		ParentGrants:    []child.Grant{{Capability: "search"}, {Capability: "read"}},
		Task:            "summarize the logs",
		RequestedGrants: []child.Grant{{Capability: "search"}, {Capability: "read"}},
		Budget:          child.Budget{MaxTokens: 1000, Timeout: time.Minute},
	}
	first, created, err := service.SpawnChild(t.Context(), spec, now)
	if err != nil || !created {
		t.Fatalf("SpawnChild: %+v, %v, %v", first, created, err)
	}
	if len(first.Grants) != 2 || first.Grants[0].Capability != "read" || first.Grants[1].Capability != "search" {
		t.Fatalf("first grants = %+v, want sorted [read search]", first.Grants)
	}
	second, created, err := service.SpawnChild(t.Context(), spec, now)
	if err != nil || created {
		t.Fatalf("replay: %+v, %v, %v", second, created, err)
	}
	if len(second.Grants) != len(first.Grants) {
		t.Fatalf("replay grants = %+v, want %+v", second.Grants, first.Grants)
	}
	for i := range first.Grants {
		if second.Grants[i] != first.Grants[i] {
			t.Fatalf("replay grants = %+v, want %+v", second.Grants, first.Grants)
		}
	}
}

func TestChildRegistryReplayReturnsPersistedGrants(t *testing.T) {
	cfgPath := writeProfileCLIConfig(t)
	loaded, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	db, err := openStorage(t.Context(), loaded.Config)
	if err != nil {
		t.Fatalf("openStorage: %v", err)
	}
	defer func() { _ = db.Close() }()
	now := time.Now().UTC()
	sessions := store.NewSessionService(db)
	for _, id := range []string{"sess-parent", "sess-child-1"} {
		if err := sessions.Create(t.Context(), &store.Session{ID: id, OwnerID: "owner-1", CreatedAt: now, UpdatedAt: now, Metadata: []byte(`{}`)}); err != nil {
			t.Fatalf("Create session %s: %v", id, err)
		}
	}
	service, err := child.NewService(newChildRegistry(db))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	spec := &child.Spec{
		ID: "ch-1", IdempotencyKey: "key-1",
		ParentSessionID: "sess-parent", ParentTurnID: "turn-1", ParentInvocation: "inv-1",
		ChildSessionID: "sess-child-1", ParentDepth: 0,
		ParentGrants:    []child.Grant{{Capability: "search"}, {Capability: "read"}},
		Task:            "summarize the logs",
		RequestedGrants: []child.Grant{{Capability: "search"}, {Capability: "read"}},
		Budget:          child.Budget{MaxTokens: 1000, Timeout: time.Minute},
	}
	first, created, err := service.SpawnChild(t.Context(), spec, now)
	if err != nil || !created || len(first.Grants) != 2 {
		t.Fatalf("SpawnChild: %+v, %v, %v", first, created, err)
	}
	narrow := *spec
	narrow.RequestedGrants = []child.Grant{{Capability: "search"}}
	replayed, created, err := service.SpawnChild(t.Context(), &narrow, now)
	if err != nil || created {
		t.Fatalf("replay: %+v, %v, %v", replayed, created, err)
	}
	if len(replayed.Grants) != 2 {
		t.Fatalf("replay must return persisted grants: %+v", replayed.Grants)
	}
	if _, found, err := newChildRegistry(db).Get(t.Context(), "missing"); err != nil || found {
		t.Fatalf("Get missing: %v, %v", found, err)
	}
	if _, _, err := newChildRegistry(db).Get(t.Context(), "ch-1"); err != nil {
		t.Fatalf("Get: %v", err)
	}
}
