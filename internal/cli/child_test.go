package cli

import (
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
