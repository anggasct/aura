package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/memory"
	"github.com/anggasct/aura/internal/principal"
	"github.com/anggasct/aura/internal/runtime"
	"github.com/anggasct/aura/internal/runtime/ingress"
	"github.com/anggasct/aura/internal/secret"
	"github.com/anggasct/aura/internal/store"
)

func journalPayload(t *testing.T, author, text string) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"content": map[string]any{"parts": []any{map[string]any{"text": text}}},
		"author":  author,
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return raw
}

func hookTurnRequest() *runtime.TurnRequest {
	return &runtime.TurnRequest{TurnID: "turn-1", SessionID: "sess-1", PrincipalID: "owner-1", Origin: runtime.OriginTerminal}
}

func TestMemoryProjectionJournalSeeded(t *testing.T) {
	ctx := context.Background()
	db := newMemoryTestDB(t)
	sessions := store.NewSessionService(db)
	events := store.NewEventStore(db)
	if err := sessions.Create(ctx, &store.Session{ID: "sess-1", OwnerID: "owner-1", Metadata: json.RawMessage(`{}`), CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("create session: %v", err)
	}
	admitted := &store.RuntimeEvent{
		ID: "ev-admit-1", SessionID: "sess-1", TurnID: "turn-1", Author: "user",
		Kind: store.EventKindADK, SchemaVersion: 1,
		Payload:   journalPayload(t, "user", "the nightly backup finished cleanly"),
		CreatedAt: time.Now().UTC(),
	}
	sequence, err := events.AppendSequenced(ctx, "sess-1", admitted)
	if err != nil {
		t.Fatalf("AppendSequenced(): %v", err)
	}
	lifecycle := &store.RuntimeEvent{
		ID: "ev-life-1", SessionID: "sess-1", TurnID: "turn-1", Author: "owner-1",
		Kind: "turn.completed", SchemaVersion: 1,
		Payload:   json.RawMessage(`{"outcome":"completed"}`),
		CreatedAt: time.Now().UTC(),
	}
	if _, err := events.AppendSequenced(ctx, "sess-1", lifecycle); err != nil {
		t.Fatalf("append lifecycle: %v", err)
	}
	listed, err := sessions.ListEvents(ctx, "sess-1", 0, 100)
	if err != nil {
		t.Fatalf("ListEvents(): %v", err)
	}
	if len(listed) != 2 {
		t.Fatalf("journal events = %d, want 2", len(listed))
	}
	service, err := memory.NewService(&memoryDocumentStore{store: store.NewMemoryStore(db)}, nil, &memory.Config{})
	if err != nil {
		t.Fatalf("NewService(): %v", err)
	}
	converted := make([]*memory.Event, 0, len(listed))
	for i := range listed {
		converted = append(converted, &memory.Event{
			ID: listed[i].ID, SessionID: listed[i].SessionID, Sequence: listed[i].Sequence,
			TurnID: listed[i].TurnID, Author: listed[i].Author, Kind: listed[i].Kind,
			Payload: listed[i].Payload, CreatedAt: listed[i].CreatedAt,
		})
	}
	result, err := service.ProjectBatch(ctx, "owner-1", converted)
	if err != nil {
		t.Fatalf("ProjectBatch(): %v", err)
	}
	if result.Indexed != 1 {
		t.Fatalf("indexed = %d, want exactly one projected doc", result.Indexed)
	}
	hits, err := store.NewMemoryStore(db).Search(ctx, &store.MemoryQuery{OwnerID: "owner-1", SessionID: "sess-1", Terms: "nightly backup", Limit: 10})
	if err != nil {
		t.Fatalf("Search(): %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("fts hits = %d, want 1", len(hits))
	}
	mark, found, err := service.Watermark(ctx, "sess-1")
	if err != nil || !found || mark != sequence {
		t.Fatalf("watermark = %d, %v, %v; want %d", mark, found, err, sequence)
	}
	replay, err := service.ProjectBatch(ctx, "owner-1", converted)
	if err != nil {
		t.Fatalf("replay ProjectBatch(): %v", err)
	}
	if replay.Indexed != 1 {
		t.Errorf("replay indexed = %d, want convergent 1", replay.Indexed)
	}
	after, err := store.NewMemoryStore(db).Search(ctx, &store.MemoryQuery{OwnerID: "owner-1", SessionID: "sess-1", Terms: "nightly backup", Limit: 10})
	if err != nil || len(after) != 1 {
		t.Fatalf("replay fts hits = %d, %v; want still one doc", len(after), err)
	}
}

func TestMemoryProjectionCrossSessionFallbackRealStore(t *testing.T) {
	ctx := context.Background()
	db := newMemoryTestDB(t)
	sessions := store.NewSessionService(db)
	for _, seed := range []struct{ session, owner string }{
		{"sess-1", "owner-1"}, {"sess-2", "owner-1"}, {"sess-9", "owner-2"},
	} {
		if err := sessions.Create(ctx, &store.Session{ID: seed.session, OwnerID: seed.owner, Metadata: json.RawMessage(`{}`), CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}); err != nil {
			t.Fatalf("create %s: %v", seed.session, err)
		}
	}
	service, err := memory.NewService(&memoryDocumentStore{store: store.NewMemoryStore(db)}, nil, &memory.Config{})
	if err != nil {
		t.Fatalf("NewService(): %v", err)
	}
	project := func(owner, session string, sequence uint64, content string) {
		t.Helper()
		event := &memory.Event{
			ID: fmt.Sprintf("ev-%s-%d", session, sequence), SessionID: session, Sequence: sequence,
			Author: "user", Kind: "adk_event", Payload: journalPayload(t, "user", content), CreatedAt: time.Now().UTC(),
		}
		if _, _, err := service.ProjectEvent(ctx, owner, event); err != nil {
			t.Fatalf("project %s/%s: %v", owner, session, err)
		}
	}
	project("owner-1", "sess-1", 1, "the nightly backup finished cleanly")
	project("owner-1", "sess-2", 1, "the nightly backup failed with errors")
	project("owner-2", "sess-9", 1, "the nightly backup finished cleanly")

	recalled, err := service.Recall(ctx, &memory.RecallRequest{OwnerID: "owner-1", AgentIDs: []string{"main"}, SessionID: "sess-1", Query: "nightly backup"})
	if err != nil {
		t.Fatalf("Recall(): %v", err)
	}
	if len(recalled) != 2 {
		t.Fatalf("documents = %d, want 2 (primary plus fallback)", len(recalled))
	}
	if recalled[0].SessionID != "sess-1" || recalled[1].SessionID != "sess-2" {
		t.Errorf("ordering = %q then %q, want sess-1 then sess-2", recalled[0].SessionID, recalled[1].SessionID)
	}
	for _, document := range recalled {
		if document.SessionID == "sess-9" {
			t.Errorf("cross-owner leak: %+v", document)
		}
		if document.FromSequence == 0 {
			t.Error("provenance missing")
		}
	}
}

func TestMemoryProjectionSecretDropRealStore(t *testing.T) {
	ctx := context.Background()
	db := newMemoryTestDB(t)
	sessions := store.NewSessionService(db)
	if err := sessions.Create(ctx, &store.Session{ID: "sess-1", OwnerID: "owner-1", Metadata: json.RawMessage(`{}`), CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("create session: %v", err)
	}
	vault := secret.New()
	vault.Set("canary", "sk-live-canary")
	service, err := memory.NewService(&memoryDocumentStore{store: store.NewMemoryStore(db)}, vault, &memory.Config{})
	if err != nil {
		t.Fatalf("NewService(): %v", err)
	}
	before := service.SecretDropCount()
	event := &memory.Event{
		ID: "ev-leak", SessionID: "sess-1", Sequence: 1, Author: "user",
		Kind: "adk_event", Payload: journalPayload(t, "user", "the key is sk-live-canary do not share"), CreatedAt: time.Now().UTC(),
	}
	result, err := service.ProjectBatch(ctx, "owner-1", []*memory.Event{event})
	if err != nil {
		t.Fatalf("ProjectBatch(): %v", err)
	}
	if result.Dropped != 1 || result.Indexed != 0 {
		t.Fatalf("result = %+v, want dropped 1 indexed 0", result)
	}
	if got := service.SecretDropCount(); got != before+1 {
		t.Errorf("drops = %d, want %d", got, before+1)
	}
	hits, err := store.NewMemoryStore(db).Search(ctx, &store.MemoryQuery{OwnerID: "owner-1", SessionID: "sess-1", Terms: "key", Limit: 10})
	if err != nil {
		t.Fatalf("Search(): %v", err)
	}
	if len(hits) != 0 {
		t.Errorf("secret-bearing hits = %d, want none", len(hits))
	}
}

func TestMemoryHookProjectsAndPurges(t *testing.T) {
	ctx := context.Background()
	db := newMemoryTestDB(t)
	sessions := store.NewSessionService(db)
	if err := sessions.Create(ctx, &store.Session{ID: "sess-1", OwnerID: "owner-1", Metadata: json.RawMessage(`{}`), CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("create session: %v", err)
	}
	service, err := memory.NewService(&memoryDocumentStore{store: store.NewMemoryStore(db)}, nil, &memory.Config{})
	if err != nil {
		t.Fatalf("NewService(): %v", err)
	}
	hook := buildMemoryHook(service, nil, nil, nil)
	hookEvents := []store.RuntimeEvent{{
		ID: "ev-hook-1", SessionID: "sess-1", Sequence: 1, TurnID: "turn-1", Author: "user",
		Kind: store.EventKindADK, SchemaVersion: 1,
		Payload:   journalPayload(t, "user", "the nightly backup finished cleanly"),
		CreatedAt: time.Now().UTC(),
	}}
	if err := hook(ctx, hookTurnRequest(), hookEvents); err != nil {
		t.Fatalf("hook(): %v", err)
	}
	hits, err := store.NewMemoryStore(db).Search(ctx, &store.MemoryQuery{OwnerID: "owner-1", SessionID: "sess-1", Terms: "nightly backup", Limit: 10})
	if err != nil || len(hits) != 1 {
		t.Fatalf("hits = %d, %v; want 1", len(hits), err)
	}
	if err := hook(ctx, nil, hookEvents); err == nil {
		t.Error("nil request accepted")
	}
}

type stubRangeSummarizer struct {
	text  string
	calls int
}

func (s *stubRangeSummarizer) Summarize(_ context.Context, _ *memory.SummaryPrompt) (string, error) {
	s.calls++
	return s.text, nil
}

func capableMemorySummaryModel() *memory.SummaryModel {
	return &memory.SummaryModel{
		Protocol:         "anthropic_messages",
		Name:             "claude-sonnet-4-20250514",
		Tokenizer:        "claude",
		ContextTokens:    200000,
		StructuredOutput: true,
	}
}

func TestMemoryHookSummarizesClosedRange(t *testing.T) {
	ctx := context.Background()
	db := newMemoryTestDB(t)
	sessions := store.NewSessionService(db)
	if err := sessions.Create(ctx, &store.Session{ID: "sess-1", OwnerID: "owner-1", Metadata: json.RawMessage(`{}`), CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("create session: %v", err)
	}
	service, err := memory.NewService(&memoryDocumentStore{store: store.NewMemoryStore(db)}, nil, &memory.Config{RangeThreshold: 3, RangeLiveWindow: 1})
	if err != nil {
		t.Fatalf("NewService(): %v", err)
	}
	summarizer := &stubRangeSummarizer{text: "archive backup digest"}
	hook := buildMemoryHook(service, capableMemorySummaryModel(), summarizer, nil)
	events := make([]store.RuntimeEvent, 0, 4)
	for i := 1; i <= 4; i++ {
		content := "archive backup old note " + string(rune('a'+i))
		if i > 2 {
			content = "live backup recent note " + string(rune('a'+i))
		}
		events = append(events, store.RuntimeEvent{
			ID: fmt.Sprintf("ev-hook-%d", i), SessionID: "sess-1", Sequence: uint64(i), TurnID: "turn-1", Author: "user",
			Kind: store.EventKindADK, SchemaVersion: 1,
			Payload:   journalPayload(t, "user", content),
			CreatedAt: time.Now().UTC(),
		})
	}
	if err := hook(ctx, hookTurnRequest(), events); err != nil {
		t.Fatalf("hook(): %v", err)
	}
	if summarizer.calls != 1 {
		t.Fatalf("summarizer calls = %d, want 1", summarizer.calls)
	}
	hits, err := store.NewMemoryStore(db).Search(ctx, &store.MemoryQuery{OwnerID: "owner-1", SessionID: "sess-1", Terms: "digest", Limit: 10})
	if err != nil || len(hits) != 1 {
		t.Fatalf("summary hits = %d, %v; want 1", len(hits), err)
	}
	if hits[0].Kind != memory.KindSummary || hits[0].PromptVersion == "" {
		t.Fatalf("summary doc = %+v, want provenance-bearing summary", hits[0])
	}
	if hits[0].FromSequence != 1 || hits[0].ToSequence != 3 {
		t.Errorf("summary range = %d-%d, want 1-3", hits[0].FromSequence, hits[0].ToSequence)
	}
	recalled, err := service.Recall(ctx, &memory.RecallRequest{OwnerID: "owner-1", AgentIDs: []string{"main"}, SessionID: "sess-1", Query: "archive"})
	if err != nil {
		t.Fatalf("Recall(): %v", err)
	}
	if len(recalled) == 0 || recalled[0].PromptVersion == "" {
		t.Fatalf("old-range recall first = %+v, want summary first", recalled)
	}
	if recalled[0].FromSequence != 1 || recalled[0].ToSequence != 3 {
		t.Errorf("recalled summary range = %d-%d, want 1-3", recalled[0].FromSequence, recalled[0].ToSequence)
	}
	for _, document := range recalled[1:] {
		if document.PromptVersion != "" {
			t.Errorf("old-range tail holds summary: %+v", recalled)
			break
		}
		if document.FromSequence > 3 {
			t.Errorf("old-range tail holds live doc: %+v", document)
		}
	}
}

func TestMemoryHookSkipsRangeSummaryWithoutSummarizer(t *testing.T) {
	ctx := context.Background()
	db := newMemoryTestDB(t)
	sessions := store.NewSessionService(db)
	if err := sessions.Create(ctx, &store.Session{ID: "sess-1", OwnerID: "owner-1", Metadata: json.RawMessage(`{}`), CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("create session: %v", err)
	}
	service, err := memory.NewService(&memoryDocumentStore{store: store.NewMemoryStore(db)}, nil, &memory.Config{RangeThreshold: 1, RangeLiveWindow: 1})
	if err != nil {
		t.Fatalf("NewService(): %v", err)
	}
	hook := buildMemoryHook(service, nil, nil, nil)
	if err := hook(ctx, hookTurnRequest(), []store.RuntimeEvent{{
		ID: "ev-hook-1", SessionID: "sess-1", Sequence: 1, TurnID: "turn-1", Author: "user",
		Kind: store.EventKindADK, SchemaVersion: 1,
		Payload:   journalPayload(t, "user", "the nightly backup finished cleanly"),
		CreatedAt: time.Now().UTC(),
	}}); err != nil {
		t.Fatalf("hook(): %v", err)
	}
	hits, err := store.NewMemoryStore(db).Search(ctx, &store.MemoryQuery{OwnerID: "owner-1", SessionID: "sess-1", Terms: "backup", Limit: 10})
	if err != nil || len(hits) != 1 {
		t.Fatalf("hits = %d, %v; want 1 raw doc and no summary", len(hits), err)
	}
	if hits[0].Kind == memory.KindSummary {
		t.Errorf("summary created without a summarizer: %+v", hits[0])
	}
}

func TestOwnerDiscordMemoryIsOwnerKeyedAndHiddenFromTrusted(t *testing.T) {
	ctx := context.Background()
	db := newMemoryTestDB(t)
	sessions := store.NewSessionService(db)
	for id, owner := range map[string]string{"dm:777": principal.OwnerID, "dm:111": "discord:111", "terminal-1": principal.OwnerID} {
		if err := sessions.Create(ctx, &store.Session{ID: id, OwnerID: owner, Metadata: json.RawMessage(`{}`), CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}); err != nil {
			t.Fatalf("create session %s: %v", id, err)
		}
	}
	service, err := memory.NewService(&memoryDocumentStore{store: store.NewMemoryStore(db)}, nil, &memory.Config{})
	if err != nil {
		t.Fatalf("NewService(): %v", err)
	}
	ownerTurn := &runtime.TurnRequest{TurnID: "turn-1", SessionID: "dm:777", PrincipalID: principal.OwnerID, PrincipalClass: principal.ClassOwner, Origin: "discord:owner"}
	if err := buildMemoryHook(service, nil, nil, nil)(ctx, ownerTurn, []store.RuntimeEvent{{
		ID: "ev-owner-1", SessionID: "dm:777", Sequence: 1, TurnID: "turn-1", Author: principal.OwnerID,
		Kind: store.EventKindADK, SchemaVersion: 1,
		Payload:   journalPayload(t, "user", "the nightly backup finished cleanly"),
		CreatedAt: time.Now().UTC(),
	}}); err != nil {
		t.Fatalf("hook(): %v", err)
	}
	inspected, err := service.Inspect(ctx, principal.OwnerID, "", "nightly backup", 10)
	if err != nil || len(inspected) != 1 {
		t.Fatalf("owner inspect = %d docs, %v; want the discord-derived doc", len(inspected), err)
	}
	provider, err := memory.NewProvider(service, memory.ProviderConfig{Scopes: defaultRecallScopes(t)})
	if err != nil {
		t.Fatalf("NewProvider(): %v", err)
	}
	recall := func(sessionID, principalID string, class principal.Class) int {
		t.Helper()
		evidence, err := provider.RecallTurn(ctx, &runtime.TurnRequest{
			TurnID: "turn-2", SessionID: sessionID, PrincipalID: principalID, PrincipalClass: class, Origin: "discord:owner", AgentID: "main",
			Parts: []runtimeingress.InputPart{{Text: "nightly backup"}},
		})
		if err != nil {
			t.Fatalf("RecallTurn(%s): %v", principalID, err)
		}
		if evidence == nil {
			return 0
		}
		return len(evidence.Documents)
	}
	if got := recall("dm:111", "discord:111", principal.ClassTrusted); got != 0 {
		t.Errorf("trusted recall returned %d owner docs, want none", got)
	}
	if got := recall("terminal-1", principal.OwnerID, principal.ClassOwner); got == 0 {
		t.Error("owner recall from the terminal missed the discord-derived doc")
	}
}

func defaultRecallScopes(t *testing.T) memory.RecallScopes {
	t.Helper()
	cfg := config.Default()
	return testRecallScopes(t, &cfg)
}
