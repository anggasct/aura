package memory

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/operation"
	"github.com/anggasct/aura/internal/platform/operation/operationtest"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/turn"
	runtimeingress "github.com/anggasct/aura/internal/platform/turn/ingress"
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

func seedModelDocument(t *testing.T, service *Service, sessionID string, sequence uint64, content string) Document {
	t.Helper()
	event := &Event{
		ID: "ev-model", SessionID: sessionID, Sequence: sequence,
		Author: "model", Kind: "adk_event",
		Payload: adkPayload(t, "model", content, false), CreatedAt: time.Now().UTC(),
	}
	doc, ok, err := service.ProjectEvent(t.Context(), principal.OwnerID, event)
	if err != nil || !ok {
		t.Fatalf("ProjectEvent(model): ok=%v err=%v", ok, err)
	}
	return doc
}

func TestRecallFromATaintedSessionTaintsBeforeChangesRelax(t *testing.T) {
	store := newFakeDocumentStore()
	service, err := NewService(store, &fakeSecrets{}, &ServiceOptions{
		TaintMarks: staticTaintMarks{"sess-tainted": 2},
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	seedModelDocument(t, service, "sess-tainted", 3, "the nightly backup finished cleanly")
	provider, err := NewProvider(service, &ProviderConfig{Scopes: testScopes})
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
		service, err := NewService(store, &fakeSecrets{}, &ServiceOptions{
			TaintMarks: staticTaintMarks{"sess-a": 5},
		})
		if err != nil {
			t.Fatalf("NewService: %v", err)
		}
		seedModelDocument(t, service, "sess-a", 5, "alpha deployment checklist")
		provider, err := NewProvider(service, &ProviderConfig{Scopes: testScopes})
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
		service, err := NewService(store, &fakeSecrets{}, &ServiceOptions{
			TaintMarks: staticTaintMarks{"sess-b": 10},
		})
		if err != nil {
			t.Fatalf("NewService: %v", err)
		}
		seedModelDocument(t, service, "sess-b", 2, "beta architecture notes")
		provider, err := NewProvider(service, &ProviderConfig{Scopes: testScopes})
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
	service, err := NewService(store, &fakeSecrets{}, &ServiceOptions{
		TaintMarks: staticTaintMarks{},
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	seedProviderDocument(t, service, "sess-clean", 1, "clean project roadmap")
	provider, err := NewProvider(service, &ProviderConfig{Scopes: testScopes})
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

	t.Run("summary stays tainted when tainted source document is screened out", func(t *testing.T) {
		secStore := newFakeDocumentStore()
		secrets := &fakeSecrets{known: map[string]bool{}}
		svc, err := NewService(secStore, secrets, &ServiceOptions{
			TaintMarks: staticTaintMarks{"sess-dirty-sec": 1},
		})
		if err != nil {
			t.Fatalf("NewService: %v", err)
		}
		seedModelDocument(t, svc, "sess-dirty-sec", 3, "deploy checklist SECRET_TOKEN_99")
		seedProviderDocument(t, svc, "sess-clean-sec", 1, "deploy checklist public step")
		secrets.known["SECRET_TOKEN_99"] = true
		p, err := NewProvider(svc, &ProviderConfig{
			Scopes:     testScopes,
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
	svc, err := NewService(memStore, &fakeSecrets{}, &ServiceOptions{
		TaintMarks: turnstore.NewTaintReader(db),
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	seedProviderDocument(t, svc, "sess-clean-ins", 1, "clean alpha note")
	seedModelDocument(t, svc, "sess-dirty-ins", 2, "dirty beta note")

	ops, err := NewOperations(&OperationDeps{
		Service: svc,
		Scopes:  testScopes,
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

func makeTurnADKEvent(t *testing.T, sessionID, turnID string, seq uint64, author string, content *genai.Content) turn.Event {
	t.Helper()
	ev, err := turnstore.RuntimeEventFromADK(sessionID, turnID, &session.Event{
		ID:          turnID + "-" + author,
		Author:      author,
		Timestamp:   time.Date(2026, 11, 28, 12, 0, int(seq), 0, time.UTC),
		LLMResponse: adkmodel.LLMResponse{Content: content},
	})
	if err != nil {
		t.Fatalf("RuntimeEventFromADK: %v", err)
	}
	ev.Sequence = seq
	return ev
}

func TestMemoryDocumentsCarryTheirTaint(t *testing.T) {
	ctx := t.Context()
	db := operationtest.OpenDatabase(t, Schema())
	sessions := turnstore.NewSessionService(db)
	events := turnstore.NewEventStore(db)
	now := time.Now().UTC()

	for _, s := range []struct {
		id, owner string
	}{
		{"sess-mix", principal.OwnerID},
		{"sess-other", "discord:222"},
		{"sess-req", principal.OwnerID},
		{"cron:job-clean:20261128T120000.000000000", principal.OwnerID},
		{"cron:job-dirty:20261128T120000.000000000", principal.OwnerID},
	} {
		if err := sessions.Create(ctx, &turn.Session{ID: s.id, OwnerID: s.owner, AgentID: "main", CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("Create(%s): %v", s.id, err)
		}
	}

	memStore, err := NewSQLiteStore(db, sessions)
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	svc, err := NewService(memStore, &fakeSecrets{}, &ServiceOptions{
		TaintMarks: turnstore.NewTaintReader(db),
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	hook := NewTurnHook(svc, nil, nil, nil)

	markPayload, _ := turn.EncodeTaintMark(turn.TaintMark{Source: turn.TaintSourceToolResult, Detail: "web_fetch"})
	t.Run("owner message stays clean while model after mark is tainted", func(t *testing.T) {
		turnEvents := []turn.Event{
			makeTurnADKEvent(t, "sess-mix", "t1", 1, "user", &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "owner early clean note"}}}),
			makeTurnADKEvent(t, "sess-mix", "t1", 2, "model", &genai.Content{Role: "model", Parts: []*genai.Part{{Text: "model early clean reply"}}}),
			{ID: "mark-mix", SessionID: "sess-mix", TurnID: "t1", Sequence: 3, Author: "system", Kind: turn.EventKindSessionTainted, SchemaVersion: 1, Payload: markPayload, CreatedAt: now},
			makeTurnADKEvent(t, "sess-mix", "t1", 4, "model", &genai.Content{Role: "model", Parts: []*genai.Part{{Text: "model tainted reply"}}}),
		}
		for i := range turnEvents {
			ev := turnEvents[i]
			ev.ID = "mix-ev-" + string(rune('a'+i))
			ev.SchemaVersion = 1
			seq, err := events.AppendSequenced(ctx, "sess-mix", &ev)
			if err != nil {
				t.Fatalf("append event %d: %v", i, err)
			}
			turnEvents[i].Sequence = seq
		}
		if err := hook.AfterTurn(ctx, &turn.TurnRequest{
			TurnID: "t1", SessionID: "sess-mix", PrincipalID: principal.OwnerID, PrincipalClass: principal.ClassOwner, Origin: turn.OriginTerminal,
		}, turnEvents); err != nil {
			t.Fatalf("AfterTurn t1: %v", err)
		}
		turn2Events := []turn.Event{
			makeTurnADKEvent(t, "sess-mix", "t2", 5, "user", &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "owner later typed message"}}}),
			makeTurnADKEvent(t, "sess-mix", "t2", 6, "model", &genai.Content{Role: "model", Parts: []*genai.Part{{Text: "model later tainted reply"}}}),
		}
		if err := hook.AfterTurn(ctx, &turn.TurnRequest{
			TurnID: "t2", SessionID: "sess-mix", PrincipalID: principal.OwnerID, PrincipalClass: principal.ClassOwner, Origin: turn.OriginTerminal,
		}, turn2Events); err != nil {
			t.Fatalf("AfterTurn t2: %v", err)
		}
		docs, err := memStore.ListDocuments(ctx, &DocumentFilter{OwnerID: principal.OwnerID, SessionID: "sess-mix", Limit: 20})
		if err != nil {
			t.Fatalf("ListDocuments: %v", err)
		}
		wantBySeq := map[int64]bool{1: false, 2: false, 4: true, 5: false, 6: true}
		if len(docs) != len(wantBySeq) {
			t.Fatalf("docs = %d, want %d", len(docs), len(wantBySeq))
		}
		for _, doc := range docs {
			if doc.Tainted == nil {
				t.Errorf("seq %d has NULL tainted, want explicit 0/1", doc.FromSequence)
				continue
			}
			if want := wantBySeq[doc.FromSequence]; *doc.Tainted != want {
				t.Errorf("seq %d (%q) tainted=%t, want %t", doc.FromSequence, doc.Content, *doc.Tainted, want)
			}
		}
	})

	t.Run("live clean owner HTTP turn stays untainted while client history taints", func(t *testing.T) {
		for _, sid := range []string{"sess-http-clean", "sess-http-hist"} {
			if err := sessions.Create(ctx, &turn.Session{ID: sid, OwnerID: principal.OwnerID, AgentID: "main", CreatedAt: now, UpdatedAt: now}); err != nil {
				t.Fatalf("Create(%s): %v", sid, err)
			}
		}
		cleanEvents := []turn.Event{
			{ID: "http-acc-1", SessionID: "sess-http-clean", TurnID: "t-http-clean", Sequence: 1, Author: principal.OwnerID, Kind: turn.EventKindTurnAccepted, SchemaVersion: 1, Payload: json.RawMessage(`{"origin":"http","principal":"local-owner"}`), CreatedAt: now},
			makeTurnADKEvent(t, "sess-http-clean", "t-http-clean", 2, "user", &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "clean http owner prompt"}}}),
		}
		if err := hook.AfterTurn(ctx, &turn.TurnRequest{
			TurnID: "t-http-clean", SessionID: "sess-http-clean", PrincipalID: principal.OwnerID, PrincipalClass: principal.ClassOwner, Origin: turn.OriginHTTP,
		}, cleanEvents); err != nil {
			t.Fatalf("AfterTurn clean http: %v", err)
		}
		cleanDoc, ok, err := memStore.GetDocument(ctx, documentID("sess-http-clean", KindEventText, 2, 2, ""))
		if err != nil || !ok || cleanDoc.Tainted == nil || *cleanDoc.Tainted {
			t.Fatalf("clean http owner doc = %+v (ok=%v err=%v), want tainted=false", cleanDoc, ok, err)
		}

		histEvents := []turn.Event{
			{ID: "http-acc-2", SessionID: "sess-http-hist", TurnID: "t-http-hist", Sequence: 1, Author: principal.OwnerID, Kind: turn.EventKindTurnAccepted, SchemaVersion: 1, Payload: json.RawMessage(`{"origin":"http","principal":"local-owner"}`), CreatedAt: now},
			makeTurnADKEvent(t, "sess-http-hist", "t-http-hist", 2, "user", &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "http prompt with client history"}}}),
		}
		if err := hook.AfterTurn(ctx, &turn.TurnRequest{
			TurnID: "t-http-hist", SessionID: "sess-http-hist", PrincipalID: principal.OwnerID, PrincipalClass: principal.ClassOwner, Origin: turn.OriginHTTP,
			UntrustedContext: &turn.UntrustedRecall{},
		}, histEvents); err != nil {
			t.Fatalf("AfterTurn http client_history: %v", err)
		}
		histDoc, ok, err := memStore.GetDocument(ctx, documentID("sess-http-hist", KindEventText, 2, 2, ""))
		if err != nil || !ok || histDoc.Tainted == nil || !*histDoc.Tainted {
			t.Fatalf("http client_history doc = %+v (ok=%v err=%v), want tainted=true", histDoc, ok, err)
		}
	})

	t.Run("non-owner user text is tainted", func(t *testing.T) {
		ev := makeTurnADKEvent(t, "sess-other", "t-other", 1, "user", &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "message from another person"}}})
		if err := hook.AfterTurn(ctx, &turn.TurnRequest{
			TurnID: "t-other", SessionID: "sess-other", PrincipalID: "discord:222", PrincipalClass: principal.ClassTrusted, Origin: "discord:home",
		}, []turn.Event{ev}); err != nil {
			t.Fatalf("AfterTurn: %v", err)
		}
		docs, err := memStore.ListDocuments(ctx, &DocumentFilter{OwnerID: "discord:222", SessionID: "sess-other", Limit: 10})
		if err != nil || len(docs) != 1 || docs[0].Tainted == nil || !*docs[0].Tainted {
			t.Fatalf("non-owner doc = %+v, err=%v, want tainted=true", docs, err)
		}
	})

	t.Run("request admission sources taint user text", func(t *testing.T) {
		cases := []struct {
			name string
			seq  uint64
			req  *turn.TurnRequest
		}{
			{
				name: "origin",
				seq:  1,
				req:  &turn.TurnRequest{TurnID: "req-1", SessionID: "sess-req", PrincipalID: principal.OwnerID, PrincipalClass: principal.ClassOwner, Origin: turn.OriginWebhook},
			},
			{
				name: "client_history",
				seq:  2,
				req:  &turn.TurnRequest{TurnID: "req-2", SessionID: "sess-req", PrincipalID: principal.OwnerID, PrincipalClass: principal.ClassOwner, Origin: turn.OriginHTTP, UntrustedContext: &turn.UntrustedRecall{}},
			},
			{
				name: "workflow_input",
				seq:  3,
				req:  &turn.TurnRequest{TurnID: "req-3", SessionID: "sess-req", PrincipalID: principal.OwnerID, PrincipalClass: principal.ClassOwner, Origin: turn.OriginTerminal, InputTainted: true},
			},
			{
				name: "relayed_input",
				seq:  4,
				req: &turn.TurnRequest{
					TurnID: "req-4", SessionID: "sess-req", PrincipalID: principal.OwnerID, PrincipalClass: principal.ClassOwner, Origin: "telegram:main",
					Parts: []runtimeingress.InputPart{{Text: "forwarded text", Source: turn.PartSourceRelayed, Detail: "telegram forward"}},
				},
			},
			{
				name: "attachment",
				seq:  5,
				req: &turn.TurnRequest{
					TurnID: "req-5", SessionID: "sess-req", PrincipalID: principal.OwnerID, PrincipalClass: principal.ClassOwner, Origin: "telegram:main",
					Parts: []runtimeingress.InputPart{{Text: "see file", Source: turn.PartSourceAttachment, Detail: "document"}},
				},
			},
		}
		for _, tc := range cases {
			ev := makeTurnADKEvent(t, "sess-req", tc.req.TurnID, tc.seq, "user", &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "user text " + tc.name}}})
			if err := hook.AfterTurn(ctx, tc.req, []turn.Event{ev}); err != nil {
				t.Fatalf("AfterTurn(%s): %v", tc.name, err)
			}
		}
		docs, err := memStore.ListDocuments(ctx, &DocumentFilter{OwnerID: principal.OwnerID, SessionID: "sess-req", Limit: 10})
		if err != nil || len(docs) != len(cases) {
			t.Fatalf("docs = %d, err=%v", len(docs), err)
		}
		for _, doc := range docs {
			if doc.Tainted == nil || !*doc.Tainted {
				t.Errorf("request-tainted doc %+v want tainted=true", doc)
			}
		}
	})

	t.Run("scheduled run output carries its taint", func(t *testing.T) {
		cleanEv := makeTurnADKEvent(t, "cron:job-clean:20261128T120000.000000000", "run-1", 1, "model", &genai.Content{Role: "model", Parts: []*genai.Part{{Text: "clean cron digest"}}})
		if err := hook.AfterTurn(ctx, &turn.TurnRequest{
			TurnID: "run-1", SessionID: "cron:job-clean:20261128T120000.000000000", PrincipalID: principal.OwnerID, PrincipalClass: principal.ClassOwner, Origin: turn.OriginSchedule,
		}, []turn.Event{cleanEv}); err != nil {
			t.Fatalf("AfterTurn clean run: %v", err)
		}
		dirtyEv := makeTurnADKEvent(t, "cron:job-dirty:20261128T120000.000000000", "run-2", 1, "model", &genai.Content{Role: "model", Parts: []*genai.Part{{Text: "dirty cron digest"}}})
		if err := hook.AfterTurn(ctx, &turn.TurnRequest{
			TurnID: "run-2", SessionID: "cron:job-dirty:20261128T120000.000000000", PrincipalID: principal.OwnerID, PrincipalClass: principal.ClassOwner, Origin: turn.OriginSchedule, WebOnly: true,
		}, []turn.Event{dirtyEv}); err != nil {
			t.Fatalf("AfterTurn dirty run: %v", err)
		}
		cleanDocs, _ := memStore.ListDocuments(ctx, &DocumentFilter{OwnerID: principal.OwnerID, SessionID: "cron:job-clean:20261128T120000.000000000", Limit: 5})
		dirtyDocs, _ := memStore.ListDocuments(ctx, &DocumentFilter{OwnerID: principal.OwnerID, SessionID: "cron:job-dirty:20261128T120000.000000000", Limit: 5})
		if len(cleanDocs) != 1 || cleanDocs[0].Tainted == nil || *cleanDocs[0].Tainted {
			t.Errorf("clean run doc = %+v, want tainted=false", cleanDocs)
		}
		if len(dirtyDocs) != 1 || dirtyDocs[0].Tainted == nil || !*dirtyDocs[0].Tainted {
			t.Errorf("dirty run doc = %+v, want tainted=true", dirtyDocs)
		}
	})

	t.Run("summary taints iff any covered document is tainted", func(t *testing.T) {
		cleanSummary, err := svc.Summarize(ctx, principal.OwnerID, "sess-mix", "clean", []RecallDocument{
			{ID: "d1", SessionID: "sess-mix", FromSequence: 1, ToSequence: 1, Content: "owner note", Tainted: false},
			{ID: "d2", SessionID: "sess-mix", FromSequence: 2, ToSequence: 2, Content: "clean reply", Tainted: false},
		}, capableModel(), staticSummarizer("clean summary text"))
		if err != nil {
			t.Fatalf("Summarize clean: %v", err)
		}
		if cleanSummary.Tainted {
			t.Error("summary of only clean docs should not be tainted")
		}
		storedClean, ok, err := memStore.GetDocument(ctx, documentID("sess-mix", KindSummary, 1, 2, defaultSummaryPromptVersion))
		if err != nil || !ok || storedClean.Tainted == nil || *storedClean.Tainted {
			t.Fatalf("stored clean summary = %+v (ok=%v err=%v), want tainted=false", storedClean, ok, err)
		}

		dirtySummary, err := svc.Summarize(ctx, principal.OwnerID, "sess-mix", "dirty", []RecallDocument{
			{ID: "d1", SessionID: "sess-mix", FromSequence: 1, ToSequence: 1, Content: "owner note", Tainted: false},
			{ID: "d4", SessionID: "sess-mix", FromSequence: 4, ToSequence: 4, Content: "tainted reply", Tainted: true},
		}, capableModel(), staticSummarizer("mixed summary text"))
		if err != nil {
			t.Fatalf("Summarize dirty: %v", err)
		}
		if !dirtySummary.Tainted {
			t.Error("summary covering a tainted doc must be tainted")
		}
		storedDirty, ok, err := memStore.GetDocument(ctx, documentID("sess-mix", KindSummary, 1, 4, defaultSummaryPromptVersion))
		if err != nil || !ok || storedDirty.Tainted == nil || !*storedDirty.Tainted {
			t.Fatalf("stored dirty summary = %+v (ok=%v err=%v), want tainted=true", storedDirty, ok, err)
		}
	})
}

func TestRecalledOwnerMessagesDoNotTaint(t *testing.T) {
	ctx := t.Context()
	db := operationtest.OpenDatabase(t, Schema())
	sessions := turnstore.NewSessionService(db)
	events := turnstore.NewEventStore(db)
	now := time.Now().UTC()

	for _, id := range []string{"sess-past", "sess-fresh"} {
		if err := sessions.Create(ctx, &turn.Session{ID: id, OwnerID: principal.OwnerID, AgentID: "main", CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("Create(%s): %v", id, err)
		}
	}
	markPayload, _ := turn.EncodeTaintMark(turn.TaintMark{Source: turn.TaintSourceToolResult, Detail: "web_fetch"})
	if _, err := events.AppendSequenced(ctx, "sess-past", &turn.Event{
		ID: "mark-past", SessionID: "sess-past", TurnID: "t1", Author: "system",
		Kind: turn.EventKindSessionTainted, SchemaVersion: 1, Payload: markPayload, CreatedAt: now,
	}); err != nil {
		t.Fatalf("append mark: %v", err)
	}

	memStore, err := NewSQLiteStore(db, sessions)
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	svc, err := NewService(memStore, &fakeSecrets{}, &ServiceOptions{
		TaintMarks: turnstore.NewTaintReader(db),
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	hook := NewTurnHook(svc, nil, nil, nil)
	turnEvents := []turn.Event{
		makeTurnADKEvent(t, "sess-past", "t2", 5, "user", &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "remember my dog is named zorro"}}}),
		makeTurnADKEvent(t, "sess-past", "t2", 6, "model", &genai.Content{Role: "model", Parts: []*genai.Part{{Text: "acknowledged nebula response"}}}),
	}
	if err := hook.AfterTurn(ctx, &turn.TurnRequest{
		TurnID: "t2", SessionID: "sess-past", PrincipalID: principal.OwnerID, PrincipalClass: principal.ClassOwner, Origin: turn.OriginTerminal,
	}, turnEvents); err != nil {
		t.Fatalf("AfterTurn: %v", err)
	}

	provider, err := NewProvider(svc, &ProviderConfig{Scopes: testScopes})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}

	ownerBlocks, err := provider.TurnContext(ctx, turnRequest("sess-fresh", "zorro"))
	if err != nil {
		t.Fatalf("TurnContext(zorro): %v", err)
	}
	if len(ownerBlocks) != 1 || !strings.Contains(ownerBlocks[0].Content, "zorro") {
		t.Fatalf("ownerBlocks = %+v, want recalled owner message", ownerBlocks)
	}
	if ownerBlocks[0].Taint != nil {
		t.Fatalf("recalled owner-typed message tainted the turn: %+v", ownerBlocks[0].Taint)
	}

	modelBlocks, err := provider.TurnContext(ctx, turnRequest("sess-fresh", "nebula"))
	if err != nil {
		t.Fatalf("TurnContext(nebula): %v", err)
	}
	if len(modelBlocks) != 1 || modelBlocks[0].Taint == nil || modelBlocks[0].Taint.Source != turn.TaintSourceRecall {
		t.Fatalf("recalled tainted model reply = %+v, want recall taint mark", modelBlocks)
	}
}

func TestUnlabelledMemoryCountsAsTainted(t *testing.T) {
	ctx := t.Context()
	db := operationtest.OpenDatabase(t, Schema())
	sessions := turnstore.NewSessionService(db)
	now := time.Now().UTC()
	if err := sessions.Create(ctx, &turn.Session{ID: "sess-null", OwnerID: principal.OwnerID, AgentID: "main", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	memStore, err := NewSQLiteStore(db, sessions)
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	if _, err := memStore.UpsertDocument(ctx, &StoredDocument{
		ID: "mem-unlabelled", OwnerID: principal.OwnerID, SessionID: "sess-null", Kind: KindEventText,
		FromSequence: 1, ToSequence: 1, Content: "legacy unlabelled quartz memo",
		TrustLabel: string(approval.TrustOwnerInput), Tainted: nil, CreatedAt: now,
	}); err != nil {
		t.Fatalf("UpsertDocument(nil tainted): %v", err)
	}

	svc, err := NewService(memStore, &fakeSecrets{}, &ServiceOptions{})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	provider, err := NewProvider(svc, &ProviderConfig{Scopes: testScopes})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}

	blocks, err := provider.TurnContext(ctx, turnRequest("sess-live", "quartz"))
	if err != nil {
		t.Fatalf("TurnContext: %v", err)
	}
	if len(blocks) != 1 || blocks[0].Taint == nil || blocks[0].Taint.Source != turn.TaintSourceRecall {
		t.Fatalf("unlabelled recall blocks = %+v, want recall taint", blocks)
	}

	evidence, err := provider.RecallTurn(ctx, turnRequest("sess-live", "quartz"))
	if err != nil {
		t.Fatalf("RecallTurn: %v", err)
	}
	if !evidence.Tainted || len(evidence.Documents) != 1 || !evidence.Documents[0].Tainted {
		t.Fatalf("unlabelled evidence = %+v, want Tainted=true", evidence)
	}

	ops, err := NewOperations(&OperationDeps{Service: svc, Scopes: testScopes})
	if err != nil {
		t.Fatalf("NewOperations: %v", err)
	}
	rig := operationtest.NewRig(t, &operationtest.Options{
		DB:       db,
		Catalogs: []operation.Catalog{Catalog()},
		Bindings: func(*operationtest.Stubs) []operation.Binding { return ops.Bindings() },
	})
	desc, _ := rig.Registry.Descriptor("memory.inspect")
	res, err := rig.Registry.Invoke(ctx, "memory.inspect", &operation.Call{
		Caller: operation.Caller{Surface: operation.SurfaceCLI, PrincipalID: principal.OwnerID, AgentID: "main"},
		Input:  json.RawMessage(`{"query":"quartz"}`),
	})
	if err != nil {
		t.Fatalf("memory.inspect: %v", err)
	}
	if !desc.ResultTaint(res.Data) {
		t.Fatalf("memory.inspect of unlabelled row did not taint: %s", res.Data)
	}
}

func TestExistingMemoryIsRelabelledOnce(t *testing.T) {
	ctx := t.Context()
	db := operationtest.OpenDatabase(t, Schema())
	sessions := turnstore.NewSessionService(db)
	events := turnstore.NewEventStore(db)
	now := time.Now().UTC()

	for _, id := range []string{"sess-rel-clean", "sess-rel-dirty", "sess-rel-legacy"} {
		if err := sessions.Create(ctx, &turn.Session{ID: id, OwnerID: principal.OwnerID, AgentID: "main", CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("Create(%s): %v", id, err)
		}
	}

	appendEv := func(sessID string, ev *turn.Event) uint64 {
		t.Helper()
		seq, err := events.AppendSequenced(ctx, sessID, ev)
		if err != nil {
			t.Fatalf("AppendSequenced(%s): %v", sessID, err)
		}
		return seq
	}

	appendEv("sess-rel-clean", &turn.Event{
		ID: "c1", SessionID: "sess-rel-clean", TurnID: "tc1", Author: principal.OwnerID,
		Kind: turn.EventKindTurnAccepted, SchemaVersion: 1, Payload: json.RawMessage(`{"origin":"terminal","principal":"local-owner"}`), CreatedAt: now,
	})
	cUserSeq := appendEv("sess-rel-clean", &turn.Event{
		ID: "c2", SessionID: "sess-rel-clean", TurnID: "tc1", Author: "user",
		Kind: "adk_event", SchemaVersion: 1, Payload: adkPayload(t, "user", "clean user note", false), CreatedAt: now,
	})
	cModelSeq := appendEv("sess-rel-clean", &turn.Event{
		ID: "c3", SessionID: "sess-rel-clean", TurnID: "tc1", Author: "model",
		Kind: "adk_event", SchemaVersion: 1, Payload: adkPayload(t, "model", "clean model reply", false), CreatedAt: now,
	})

	dUserEarly := appendEv("sess-rel-dirty", &turn.Event{
		ID: "d1", SessionID: "sess-rel-dirty", TurnID: "td1", Author: "user",
		Kind: "adk_event", SchemaVersion: 1, Payload: adkPayload(t, "user", "early owner text", false), CreatedAt: now,
	})
	markPayload, _ := turn.EncodeTaintMark(turn.TaintMark{Source: turn.TaintSourceToolResult, Detail: "web_fetch"})
	appendEv("sess-rel-dirty", &turn.Event{
		ID: "d2", SessionID: "sess-rel-dirty", TurnID: "td1", Author: "system",
		Kind: turn.EventKindSessionTainted, SchemaVersion: 1, Payload: markPayload, CreatedAt: now,
	})
	dModelAfter := appendEv("sess-rel-dirty", &turn.Event{
		ID: "d3", SessionID: "sess-rel-dirty", TurnID: "td1", Author: "model",
		Kind: "adk_event", SchemaVersion: 1, Payload: adkPayload(t, "model", "model after fetch", false), CreatedAt: now,
	})
	dUserLate := appendEv("sess-rel-dirty", &turn.Event{
		ID: "d4", SessionID: "sess-rel-dirty", TurnID: "td2", Author: "user",
		Kind: "adk_event", SchemaVersion: 1, Payload: adkPayload(t, "user", "later owner typed note", false), CreatedAt: now,
	})
	relayedMark, _ := turn.EncodeTaintMark(turn.TaintMark{Source: turn.TaintSourceRelayedInput, Detail: "telegram forward"})
	appendEv("sess-rel-dirty", &turn.Event{
		ID: "d5", SessionID: "sess-rel-dirty", TurnID: "td3", Author: "system",
		Kind: turn.EventKindSessionTainted, SchemaVersion: 1, Payload: relayedMark, CreatedAt: now,
	})
	dUserRelayed := appendEv("sess-rel-dirty", &turn.Event{
		ID: "d6", SessionID: "sess-rel-dirty", TurnID: "td3", Author: "user",
		Kind: "adk_event", SchemaVersion: 1, Payload: adkPayload(t, "user", "forwarded message body", false), CreatedAt: now,
	})

	appendEv("sess-rel-legacy", &turn.Event{
		ID: "l1", SessionID: "sess-rel-legacy", TurnID: "tl1", Author: "system",
		Kind: "tool.completed", SchemaVersion: 1, Payload: json.RawMessage(`{"tool_name":"web_fetch","status":"ok"}`), CreatedAt: now,
	})
	lModelAfter := appendEv("sess-rel-legacy", &turn.Event{
		ID: "l2", SessionID: "sess-rel-legacy", TurnID: "tl1", Author: "model",
		Kind: "adk_event", SchemaVersion: 1, Payload: adkPayload(t, "model", "legacy derived output", false), CreatedAt: now,
	})

	memStore, err := NewSQLiteStore(db, sessions)
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	prelabelledClean := false
	seedDocs := []StoredDocument{
		{ID: "doc-c-user", OwnerID: principal.OwnerID, SessionID: "sess-rel-clean", Kind: KindEventText, FromSequence: int64(cUserSeq), ToSequence: int64(cUserSeq), Content: "clean user note", TrustLabel: string(approval.TrustOwnerInput), CreatedAt: now},
		{ID: "doc-c-model", OwnerID: principal.OwnerID, SessionID: "sess-rel-clean", Kind: KindEventText, FromSequence: int64(cModelSeq), ToSequence: int64(cModelSeq), Content: "clean model reply", TrustLabel: string(approval.TrustDerivedUntrusted), CreatedAt: now},
		{ID: "doc-c-sum", OwnerID: principal.OwnerID, SessionID: "sess-rel-clean", Kind: KindSummary, FromSequence: int64(cUserSeq), ToSequence: int64(cModelSeq), Content: "clean summary", TrustLabel: string(approval.TrustDerivedUntrusted), PromptVersion: "v1", CreatedAt: now},
		{ID: "doc-d-early", OwnerID: principal.OwnerID, SessionID: "sess-rel-dirty", Kind: KindEventText, FromSequence: int64(dUserEarly), ToSequence: int64(dUserEarly), Content: "early owner text", TrustLabel: string(approval.TrustOwnerInput), CreatedAt: now},
		{ID: "doc-d-model", OwnerID: principal.OwnerID, SessionID: "sess-rel-dirty", Kind: KindEventText, FromSequence: int64(dModelAfter), ToSequence: int64(dModelAfter), Content: "model after fetch", TrustLabel: string(approval.TrustDerivedUntrusted), CreatedAt: now},
		{ID: "doc-d-late-user", OwnerID: principal.OwnerID, SessionID: "sess-rel-dirty", Kind: KindEventText, FromSequence: int64(dUserLate), ToSequence: int64(dUserLate), Content: "later owner typed note", TrustLabel: string(approval.TrustOwnerInput), CreatedAt: now},
		{ID: "doc-d-relayed", OwnerID: principal.OwnerID, SessionID: "sess-rel-dirty", Kind: KindEventText, FromSequence: int64(dUserRelayed), ToSequence: int64(dUserRelayed), Content: "forwarded message body", TrustLabel: string(approval.TrustOwnerInput), CreatedAt: now},
		{ID: "doc-d-sum", OwnerID: principal.OwnerID, SessionID: "sess-rel-dirty", Kind: KindSummary, FromSequence: int64(dUserEarly), ToSequence: int64(dUserRelayed), Content: "dirty summary", TrustLabel: string(approval.TrustDerivedUntrusted), PromptVersion: "v1", CreatedAt: now},
		{ID: "doc-l-model", OwnerID: principal.OwnerID, SessionID: "sess-rel-legacy", Kind: KindEventText, FromSequence: int64(lModelAfter), ToSequence: int64(lModelAfter), Content: "legacy derived output", TrustLabel: string(approval.TrustDerivedUntrusted), CreatedAt: now},
		{ID: "doc-prelabelled", OwnerID: principal.OwnerID, SessionID: "sess-rel-dirty", Kind: KindEventText, FromSequence: 99, ToSequence: 99, Content: "already labelled", TrustLabel: string(approval.TrustDerivedUntrusted), Tainted: &prelabelledClean, CreatedAt: now},
	}
	for i := range seedDocs {
		if _, err := memStore.UpsertDocument(ctx, &seedDocs[i]); err != nil {
			t.Fatalf("UpsertDocument(%s): %v", seedDocs[i].ID, err)
		}
	}

	first, err := RelabelUnlabelled(ctx, db, &RelabelOptions{BatchSize: 2})
	if err != nil {
		t.Fatalf("RelabelUnlabelled first: %v", err)
	}
	if first != 9 {
		t.Fatalf("first relabelled = %d, want 9", first)
	}

	want := map[string]bool{
		"doc-c-user":      false,
		"doc-c-model":     false,
		"doc-c-sum":       false,
		"doc-d-early":     false,
		"doc-d-model":     true,
		"doc-d-late-user": false,
		"doc-d-relayed":   true,
		"doc-d-sum":       true,
		"doc-l-model":     true,
		"doc-prelabelled": false,
	}
	for id, wantTainted := range want {
		doc, ok, err := memStore.GetDocument(ctx, id)
		if err != nil || !ok {
			t.Fatalf("GetDocument(%s): ok=%v err=%v", id, ok, err)
		}
		if doc.Tainted == nil {
			t.Errorf("%s stayed NULL after relabelling", id)
			continue
		}
		if *doc.Tainted != wantTainted {
			t.Errorf("%s tainted=%t, want %t", id, *doc.Tainted, wantTainted)
		}
	}

	second, err := RelabelUnlabelled(ctx, db, &RelabelOptions{BatchSize: 2})
	if err != nil {
		t.Fatalf("RelabelUnlabelled second: %v", err)
	}
	if second != 0 {
		t.Fatalf("second relabelled = %d, want 0 (idempotent)", second)
	}

	if _, err := RelabelUnlabelled(ctx, db, &RelabelOptions{BatchSize: -1}); !fault.HasCode(err, ErrorCodeInvalidArgument) {
		t.Fatalf("negative batch size err = %v, want %s", err, ErrorCodeInvalidArgument)
	}

	if _, err := memStore.UpsertDocument(ctx, &StoredDocument{
		ID: "doc-missing-event", OwnerID: principal.OwnerID, SessionID: "sess-rel-clean", Kind: KindEventText,
		FromSequence: 77, ToSequence: 77, Content: "owner input missing backing runtime_event",
		TrustLabel: string(approval.TrustOwnerInput), CreatedAt: now,
	}); err != nil {
		t.Fatalf("UpsertDocument(missing event): %v", err)
	}
	missingCount, err := RelabelUnlabelled(ctx, db, &RelabelOptions{BatchSize: 2})
	if err != nil || missingCount != 1 {
		t.Fatalf("RelabelUnlabelled(missing event) = %d, %v; want 1, nil", missingCount, err)
	}
	missingDoc, ok, err := memStore.GetDocument(ctx, "doc-missing-event")
	if err != nil || !ok || missingDoc.Tainted == nil || !*missingDoc.Tainted {
		t.Fatalf("doc-missing-event = %+v (ok=%v err=%v), want fail-closed tainted=true", missingDoc, ok, err)
	}
}
