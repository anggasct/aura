package memory

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/operation"
	"github.com/anggasct/aura/internal/platform/operation/operationtest"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/turn"
	turnstore "github.com/anggasct/aura/internal/platform/turn/store"
)

type staticTaintMarks map[string]uint64

func (m staticTaintMarks) SessionMarks(_ context.Context, sessionIDs []string) (map[string]uint64, error) {
	out := make(map[string]uint64, len(sessionIDs))
	for _, id := range sessionIDs {
		if seq, ok := m[id]; ok {
			out[id] = seq
		}
	}
	return out, nil
}

type staticSummarizer string

func (s staticSummarizer) Summarize(_ context.Context, _ *SummaryPrompt) (string, error) {
	return string(s), nil
}

func TestRecallFromATaintedSessionTaintsBeforeChangesRelax(t *testing.T) {
	store := newFakeDocumentStore()
	service := providerService(t, store, &observationSink{})
	seedProviderDocument(t, service, "sess-tainted", 3, "the nightly backup finished cleanly")
	provider, err := NewProvider(service, &ProviderConfig{
		Scopes:     testScopes,
		TaintMarks: staticTaintMarks{"sess-tainted": 2},
	})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	blocks, err := provider.TurnContext(t.Context(), turnRequest("sess-live", "nightly backup"))
	if err != nil {
		t.Fatalf("TurnContext: %v", err)
	}
	if len(blocks) == 0 || blocks[0].Taint == nil || blocks[0].Taint.Source != turn.TaintSourceRecall {
		t.Fatalf("blocks = %+v, want recall taint mark", blocks)
	}
}

func TestRecalledTaintedMemoryTaintsTheTurn(t *testing.T) {
	t.Run("event_text at or after session first mark taints", func(t *testing.T) {
		store := newFakeDocumentStore()
		service := providerService(t, store, &observationSink{})
		seedProviderDocument(t, service, "sess-a", 5, "alpha deployment checklist")
		provider, err := NewProvider(service, &ProviderConfig{
			Scopes:     testScopes,
			TaintMarks: staticTaintMarks{"sess-a": 5},
		})
		if err != nil {
			t.Fatalf("NewProvider: %v", err)
		}
		blocks, err := provider.TurnContext(t.Context(), turnRequest("sess-now", "alpha deployment checklist"))
		if err != nil {
			t.Fatalf("TurnContext: %v", err)
		}
		if len(blocks) == 0 || blocks[0].Taint == nil || blocks[0].Taint.Source != turn.TaintSourceRecall {
			t.Fatalf("blocks = %+v, want recall mark", blocks)
		}
	})

	t.Run("event_text before session first mark stays clean", func(t *testing.T) {
		store := newFakeDocumentStore()
		service := providerService(t, store, &observationSink{})
		seedProviderDocument(t, service, "sess-b", 2, "beta architecture notes")
		provider, err := NewProvider(service, &ProviderConfig{
			Scopes:     testScopes,
			TaintMarks: staticTaintMarks{"sess-b": 10},
		})
		if err != nil {
			t.Fatalf("NewProvider: %v", err)
		}
		blocks, err := provider.TurnContext(t.Context(), turnRequest("sess-now", "beta architecture notes"))
		if err != nil {
			t.Fatalf("TurnContext: %v", err)
		}
		if len(blocks) == 0 || blocks[0].Content == "" {
			t.Fatal("expected recall block")
		}
		if blocks[0].Taint != nil {
			t.Fatalf("blocks[0].Taint = %+v, want nil for document before session taint mark", blocks[0].Taint)
		}
	})
}

func TestRecallOfACleanSessionDoesNotTaint(t *testing.T) {
	store := newFakeDocumentStore()
	service := providerService(t, store, &observationSink{})
	seedProviderDocument(t, service, "sess-clean", 1, "clean project roadmap")
	provider, err := NewProvider(service, &ProviderConfig{
		Scopes:     testScopes,
		TaintMarks: staticTaintMarks{},
	})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	blocks, err := provider.TurnContext(t.Context(), turnRequest("sess-now", "clean project roadmap"))
	if err != nil {
		t.Fatalf("TurnContext: %v", err)
	}
	if len(blocks) == 0 || blocks[0].Content == "" {
		t.Fatal("expected recall block for clean session")
	}
	if blocks[0].Taint != nil {
		t.Fatalf("blocks[0].Taint = %+v, want nil for clean session recall", blocks[0].Taint)
	}

	t.Run("nil taintMarks fails closed", func(t *testing.T) {
		failClosedProvider, err := NewProvider(service, &ProviderConfig{
			Scopes: testScopes,
		})
		if err != nil {
			t.Fatalf("NewProvider: %v", err)
		}
		gotBlocks, err := failClosedProvider.TurnContext(t.Context(), turnRequest("sess-now", "clean project roadmap"))
		if err != nil {
			t.Fatalf("TurnContext: %v", err)
		}
		if len(gotBlocks) == 0 || gotBlocks[0].Taint == nil || gotBlocks[0].Taint.Source != turn.TaintSourceRecall {
			t.Fatalf("blocks with nil taintMarks = %+v, want fail-closed recall taint", gotBlocks)
		}
	})

	t.Run("summary stays tainted when tainted source document is screened out", func(t *testing.T) {
		secStore := newFakeDocumentStore()
		secrets := &fakeSecrets{known: map[string]bool{}}
		svc, err := NewService(secStore, secrets, &ServiceOptions{})
		if err != nil {
			t.Fatalf("NewService: %v", err)
		}
		seedProviderDocument(t, svc, "sess-dirty-sec", 3, "deploy checklist SECRET_TOKEN_99")
		seedProviderDocument(t, svc, "sess-clean-sec", 1, "deploy checklist public step")
		secrets.known["SECRET_TOKEN_99"] = true
		p, err := NewProvider(svc, &ProviderConfig{
			Scopes:     testScopes,
			TaintMarks: staticTaintMarks{"sess-dirty-sec": 1},
			Model:      capableModel(),
			Summarizer: staticSummarizer("Combined deploy summary"),
		})
		if err != nil {
			t.Fatalf("NewProvider: %v", err)
		}
		gotBlocks, err := p.TurnContext(t.Context(), turnRequest("sess-now", "deploy checklist"))
		if err != nil {
			t.Fatalf("TurnContext: %v", err)
		}
		if len(gotBlocks) == 0 || gotBlocks[0].Taint == nil || gotBlocks[0].Taint.Source != turn.TaintSourceRecall {
			t.Fatalf("blocks after screening tainted doc = %+v, want recall taint preserved for summary", gotBlocks)
		}
	})
}

func TestMemoryInspectTaintsByRow(t *testing.T) {
	ctx := t.Context()
	db := operationtest.OpenDatabase(t, Schema())
	sessions := turnstore.NewSessionService(db)
	events := turnstore.NewEventStore(db)
	now := time.Now().UTC()

	for _, id := range []string{"sess-clean-ins", "sess-dirty-ins"} {
		if err := sessions.Create(ctx, &turn.Session{ID: id, OwnerID: principal.OwnerID, AgentID: "main", CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("Create(%s): %v", id, err)
		}
	}
	markPayload, _ := turn.EncodeTaintMark(turn.TaintMark{Source: turn.TaintSourceToolResult, Detail: "web_fetch"})
	if _, err := events.AppendSequenced(ctx, "sess-dirty-ins", &turn.Event{
		ID: "mark-dirty", SessionID: "sess-dirty-ins", TurnID: "turn-1", Author: "system",
		Kind: turn.EventKindSessionTainted, SchemaVersion: 1, Payload: markPayload, CreatedAt: now,
	}); err != nil {
		t.Fatalf("append mark: %v", err)
	}

	memStore, err := NewSQLiteStore(db, sessions)
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	svc, err := NewService(memStore, &fakeSecrets{}, &ServiceOptions{})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	seedProviderDocument(t, svc, "sess-clean-ins", 1, "clean alpha note")
	seedProviderDocument(t, svc, "sess-dirty-ins", 2, "dirty beta note")

	ops, err := NewOperations(&OperationDeps{
		Service:    svc,
		Scopes:     testScopes,
		TaintMarks: turnstore.NewTaintReader(db),
	})
	if err != nil {
		t.Fatalf("NewOperations: %v", err)
	}
	rig := operationtest.NewRig(t, &operationtest.Options{
		DB:       db,
		Catalogs: []operation.Catalog{Catalog()},
		Bindings: func(*operationtest.Stubs) []operation.Binding { return ops.Bindings() },
	})
	desc, _ := rig.Registry.Descriptor("memory.inspect")
	caller := operation.Caller{Surface: operation.SurfaceCLI, PrincipalID: principal.OwnerID, AgentID: "main"}

	cleanRes, err := rig.Registry.Invoke(ctx, "memory.inspect", &operation.Call{
		Caller: caller,
		Input:  json.RawMessage(`{"query":"alpha"}`),
	})
	if err != nil {
		t.Fatalf("inspect alpha: %v", err)
	}
	if desc.ResultTaint(cleanRes.Data) {
		t.Fatalf("memory.inspect of clean session tainted: %s", cleanRes.Data)
	}

	dirtyRes, err := rig.Registry.Invoke(ctx, "memory.inspect", &operation.Call{
		Caller: caller,
		Input:  json.RawMessage(`{"query":"beta"}`),
	})
	if err != nil {
		t.Fatalf("inspect beta: %v", err)
	}
	if !desc.ResultTaint(dirtyRes.Data) {
		t.Fatalf("memory.inspect of tainted session did not taint: %s", dirtyRes.Data)
	}

	if !desc.ResultTaint(json.RawMessage(`not-json`)) {
		t.Error("unparsable memory.inspect output did not fail closed")
	}
}
