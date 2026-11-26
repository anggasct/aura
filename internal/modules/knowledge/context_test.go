package knowledge

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/durable"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/turn"
	runtimeingress "github.com/anggasct/aura/internal/platform/turn/ingress"
)

const contextTestText = "REFUND-WINDOW-IS-THIRTY-DAYS"

type contextTestIndex struct {
	mu      sync.Mutex
	hits    []Hit
	err     error
	wait    bool
	calls   int
	scopes  [][]string
	queries [][]string
}

func (i *contextTestIndex) ScopeChunks(context.Context, []string) (int, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.err != nil {
		return 0, i.err
	}
	return max(len(i.hits), 1), nil
}

func (i *contextTestIndex) TermFrequency(context.Context, []string, string, int) (int, error) {
	return 1, nil
}

func (i *contextTestIndex) Search(ctx context.Context, scope, terms []string, _ int) ([]Hit, error) {
	i.mu.Lock()
	i.calls++
	i.scopes = append(i.scopes, slices.Clone(scope))
	i.queries = append(i.queries, slices.Clone(terms))
	hits, err, wait := slices.Clone(i.hits), i.err, i.wait
	i.mu.Unlock()
	if wait {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return hits, err
}

type contextJournalInvocation struct {
	mu      sync.Mutex
	journal map[string][]byte
}

func (j *contextJournalInvocation) Run() durable.RunRef { return durable.RunRef{} }
func (j *contextJournalInvocation) Payload() []byte     { return nil }
func (j *contextJournalInvocation) Signal(context.Context, string) ([]byte, bool) {
	return nil, false
}
func (j *contextJournalInvocation) Sleep(time.Duration) error            { return nil }
func (j *contextJournalInvocation) Timer(time.Duration) <-chan time.Time { return nil }
func (j *contextJournalInvocation) Wait(context.Context, string, time.Duration) (payload []byte, timedOut, ok bool) {
	return nil, false, false
}

func (j *contextJournalInvocation) RunAction(ctx context.Context, key string, fn func(ctx context.Context) ([]byte, error)) ([]byte, error) {
	j.mu.Lock()
	if out, ok := j.journal[key]; ok {
		j.mu.Unlock()
		return out, nil
	}
	j.mu.Unlock()
	out, err := fn(ctx)
	if err != nil {
		return nil, err
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.journal == nil {
		j.journal = map[string][]byte{}
	}
	j.journal[key] = out
	return out, nil
}

func newTestContextProvider(t *testing.T, index Index, opts ...RetrieverOption) *ContextProvider {
	t.Helper()
	retriever, err := NewRetriever(index, &fakeSecrets{}, opts...)
	if err != nil {
		t.Fatalf("NewRetriever: %v", err)
	}
	provider, err := NewContextProvider(retriever, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("NewContextProvider: %v", err)
	}
	return provider
}

func contextTurnRequest(collections ...string) *turn.TurnRequest {
	return &turn.TurnRequest{
		TurnID:         "turn-1",
		SessionID:      "session-1",
		PrincipalID:    "user-1",
		PrincipalClass: principal.ClassOwner,
		Origin:         turn.OriginTerminal,
		Parts:          []runtimeingress.InputPart{{Text: "What is the refund window?"}},
		Knowledge:      collections,
	}
}

func sampleContextHits() []Hit {
	return []Hit{
		{ChunkID: "kbc_1", Collection: "support", Label: "faq.md", Heading: "Refunds", Ordinal: 2, Content: contextTestText},
		{ChunkID: "kbc_2", Collection: "support", Label: "faq.md", Heading: "Refunds", Ordinal: 3, Content: "Contact support for exceptions."},
	}
}

func decodeContextRetrieved(t *testing.T, event *turn.Event) knowledgeRetrievedPayload {
	t.Helper()
	var payload knowledgeRetrievedPayload
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		t.Fatalf("decode %s: %v", event.Payload, err)
	}
	return payload
}

func TestContextProviderRejectsNilInputs(t *testing.T) {
	if _, err := NewContextProvider(nil, nil); !fault.HasCode(err, ErrorCodeInvalidArgument) {
		t.Fatalf("NewContextProvider(nil) = %v, want %q", err, ErrorCodeInvalidArgument)
	}
	provider := newTestContextProvider(t, &contextTestIndex{})
	if provider.Required(contextTurnRequest("support")) {
		t.Fatal("Required() = true, want false")
	}
	if _, err := provider.TurnContext(t.Context(), nil); !fault.HasCode(err, ErrorCodeInvalidArgument) {
		t.Fatalf("TurnContext(nil) = %v, want %q", err, ErrorCodeInvalidArgument)
	}
}

func TestKnowledgeContextIsAnEphemeralUntrustedPartAndNeverPersists(t *testing.T) {
	index := &contextTestIndex{hits: sampleContextHits()}
	provider := newTestContextProvider(t, index)
	blocks, err := provider.TurnContext(t.Context(), contextTurnRequest("support"))
	if err != nil {
		t.Fatalf("TurnContext: %v", err)
	}
	if len(blocks) != 1 {
		t.Fatalf("blocks = %d, want 1", len(blocks))
	}
	block := blocks[0]
	if block.Order != turn.ContextOrderKnowledge {
		t.Errorf("Order = %d, want %d", block.Order, turn.ContextOrderKnowledge)
	}
	if !strings.HasPrefix(block.Content, ContextStart) || !strings.HasSuffix(block.Content, ContextEnd) || !strings.Contains(block.Content, contextTestText) {
		t.Fatalf("unexpected content block:\n%s", block.Content)
	}
	if block.Caveat != Caveat {
		t.Errorf("Caveat = %q, want %q", block.Caveat, Caveat)
	}
	for i := range block.Events {
		if strings.Contains(string(block.Events[i].Payload), contextTestText) {
			t.Fatalf("knowledge text persisted in event %s: %s", block.Events[i].Kind, block.Events[i].Payload)
		}
	}
	if index.calls != 1 || !slices.Equal(index.scopes[0], []string{"support"}) {
		t.Errorf("index calls = %d scopes = %v", index.calls, index.scopes)
	}
}

func TestKnowledgeRetrievedEventCarriesMetadataOnly(t *testing.T) {
	index := &contextTestIndex{hits: sampleContextHits()}
	provider := newTestContextProvider(t, index)
	blocks, err := provider.TurnContext(t.Context(), contextTurnRequest("support"))
	if err != nil {
		t.Fatalf("TurnContext: %v", err)
	}
	if len(blocks) != 1 || len(blocks[0].Events) != 1 {
		t.Fatalf("blocks = %+v", blocks)
	}
	ev := blocks[0].Events[0]
	if ev.Kind != EventKindRetrieved || ev.Author != turn.AuthorSystem {
		t.Fatalf("event = %+v", ev)
	}
	payload := decodeContextRetrieved(t, &ev)
	if payload.Outcome != knowledgeOutcomeOK || !slices.Equal(payload.ChunkIDs, []string{"kbc_1", "kbc_2"}) || payload.Counts.Retrieved != 2 || payload.Counts.Rendered != 2 || payload.DurationMS < 0 || payload.Mode != ModeLexical {
		t.Errorf("payload = %+v", payload)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(ev.Payload, &raw); err != nil || len(raw) != 5 {
		t.Errorf("payload keys = %v, want chunk_ids, counts, duration_ms, mode and outcome only", raw)
	}
	for _, text := range []string{contextTestText, "passage", "Refunds"} {
		if strings.Contains(string(ev.Payload), text) {
			t.Errorf("event payload carries text %q: %s", text, ev.Payload)
		}
	}
}

func TestKnowledgeContextIsSkippedWithoutAStampedList(t *testing.T) {
	index := &contextTestIndex{hits: sampleContextHits()}
	provider := newTestContextProvider(t, index)
	blocks, err := provider.TurnContext(t.Context(), contextTurnRequest())
	if err != nil {
		t.Fatalf("TurnContext: %v", err)
	}
	if index.calls != 0 || len(blocks) != 0 {
		t.Fatalf("calls = %d blocks = %d, want none for a turn without a list", index.calls, len(blocks))
	}
}

func TestKnowledgeFailureLeavesTheTurnRunning(t *testing.T) {
	tests := map[string]struct {
		index       *contextTestIndex
		wantOutcome string
	}{
		"error":   {index: &contextTestIndex{err: errors.New("store busy")}, wantOutcome: knowledgeOutcomeError},
		"empty":   {index: &contextTestIndex{}, wantOutcome: knowledgeOutcomeEmpty},
		"timeout": {index: &contextTestIndex{wait: true}, wantOutcome: knowledgeOutcomeTimeout},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			provider := newTestContextProvider(t, test.index)
			blocks, err := provider.TurnContext(t.Context(), contextTurnRequest("support"))
			if err != nil {
				t.Fatalf("TurnContext returned error: %v", err)
			}
			if len(blocks) != 1 || blocks[0].Content != "" {
				t.Fatalf("blocks = %+v, want caveat-only block without content", blocks)
			}
			if blocks[0].Caveat != Caveat {
				t.Error("the caveat must stay on a turn that can still call kb_search")
			}
			if len(blocks[0].Events) != 1 {
				t.Fatalf("retrieved events = %d, want 1", len(blocks[0].Events))
			}
			payload := decodeContextRetrieved(t, &blocks[0].Events[0])
			if payload.Outcome != test.wantOutcome || len(payload.ChunkIDs) != 0 || payload.Counts.Rendered != 0 {
				t.Errorf("payload = %+v, want outcome %s with no chunks", payload, test.wantOutcome)
			}
		})
	}
}

func TestKnowledgeContextIsIndependentOfRecall(t *testing.T) {
	index := &contextTestIndex{hits: sampleContextHits()}
	provider := newTestContextProvider(t, index)
	req := contextTurnRequest("support")
	req.UntrustedContext = &turn.UntrustedRecall{Query: "refund", Summary: "prior note"}
	blocks, err := provider.TurnContext(t.Context(), req)
	if err != nil {
		t.Fatalf("TurnContext: %v", err)
	}
	if index.calls != 1 || len(blocks) != 1 || blocks[0].Content == "" || len(blocks[0].Events) != 1 {
		t.Fatalf("calls = %d blocks = %+v: knowledge must not depend on the recall branch", index.calls, blocks)
	}
}

func TestDurableKnowledgeContextReplaysTheJournaledBlock(t *testing.T) {
	index := &contextTestIndex{hits: sampleContextHits()}
	provider := newTestContextProvider(t, index)
	inv := &contextJournalInvocation{}
	req := contextTurnRequest("support")
	firstBlocks, err := provider.TurnContext(durable.WithTurnScope(t.Context(), durable.NewTurnScope(inv)), req)
	if err != nil {
		t.Fatalf("first TurnContext: %v", err)
	}
	index.mu.Lock()
	index.hits = []Hit{{ChunkID: "kbc_9", Collection: "support", Label: "faq.md", Heading: "Changed", Ordinal: 1, Content: "CHANGED-AFTER-RESTART"}}
	index.mu.Unlock()
	replayedBlocks, err := provider.TurnContext(durable.WithTurnScope(t.Context(), durable.NewTurnScope(inv)), req)
	if err != nil {
		t.Fatalf("replayed TurnContext: %v", err)
	}
	if len(firstBlocks) != 1 || len(replayedBlocks) != 1 {
		t.Fatalf("first = %d replayed = %d", len(firstBlocks), len(replayedBlocks))
	}
	if firstBlocks[0].Content == "" || replayedBlocks[0].Content != firstBlocks[0].Content {
		t.Errorf("replayed content = %q, want %q", replayedBlocks[0].Content, firstBlocks[0].Content)
	}
	firstEv := decodeContextRetrieved(t, &firstBlocks[0].Events[0])
	replayedEv := decodeContextRetrieved(t, &replayedBlocks[0].Events[0])
	if !slices.Equal(replayedEv.ChunkIDs, firstEv.ChunkIDs) || replayedEv.Counts != firstEv.Counts || replayedEv.DurationMS != firstEv.DurationMS || replayedEv.Outcome != firstEv.Outcome {
		t.Errorf("replayed = %+v, want %+v", replayedEv, firstEv)
	}
	if index.calls != 1 {
		t.Errorf("index calls = %d, want the replay served from the journal", index.calls)
	}
}

func TestDurableKnowledgeFailureIsJournaledAsAnEmptyBlock(t *testing.T) {
	index := &contextTestIndex{err: errors.New("store busy")}
	provider := newTestContextProvider(t, index)
	inv := &contextJournalInvocation{}
	req := contextTurnRequest("support")
	firstBlocks, err := provider.TurnContext(durable.WithTurnScope(t.Context(), durable.NewTurnScope(inv)), req)
	if err != nil {
		t.Fatalf("first TurnContext: %v", err)
	}
	index.mu.Lock()
	index.err, index.hits = nil, sampleContextHits()
	index.mu.Unlock()
	replayedBlocks, err := provider.TurnContext(durable.WithTurnScope(t.Context(), durable.NewTurnScope(inv)), req)
	if err != nil {
		t.Fatalf("replayed TurnContext: %v", err)
	}
	firstEv := decodeContextRetrieved(t, &firstBlocks[0].Events[0])
	replayedEv := decodeContextRetrieved(t, &replayedBlocks[0].Events[0])
	if firstBlocks[0].Content != "" || firstEv.Outcome != knowledgeOutcomeError || replayedBlocks[0].Content != "" || replayedEv.Outcome != knowledgeOutcomeError {
		t.Fatalf("first = %+v replayed = %+v, want failure journaled and never re-queried", firstEv, replayedEv)
	}
	if index.calls != 0 {
		t.Errorf("index search calls = %d, want 0 (failed in ScopeChunks)", index.calls)
	}
	if _, journaled := inv.journal[knowledgeAction]; !journaled {
		t.Error("the failure was not journaled")
	}
}

func TestDurableKnowledgeSkipsTheActionForDescriptorsWithoutAList(t *testing.T) {
	index := &contextTestIndex{hits: sampleContextHits()}
	provider := newTestContextProvider(t, index)
	inv := &contextJournalInvocation{}
	blocks, err := provider.TurnContext(durable.WithTurnScope(t.Context(), durable.NewTurnScope(inv)), contextTurnRequest())
	if err != nil {
		t.Fatalf("TurnContext: %v", err)
	}
	if len(blocks) != 0 || len(inv.journal) != 0 || index.calls != 0 {
		t.Fatalf("blocks = %d journal = %v calls = %d, want the action skipped so in-flight turns replay unchanged", len(blocks), inv.journal, index.calls)
	}
}

func TestKnowledgeJournalRejectsUnknownFieldsAndFailsClosed(t *testing.T) {
	fallback := knowledgeJournal{ChunkIDs: []string{}, Outcome: knowledgeOutcomeError}
	for name, raw := range map[string]string{
		"unknown field": `{"block":"x","outcome":"ok","extra":1}`,
		"not json":      `block`,
		"trailing":      `{"outcome":"ok"} {"outcome":"ok"}`,
	} {
		t.Run(name, func(t *testing.T) {
			got := decodeKnowledgeJournal([]byte(raw), &fallback)
			if got.Block != "" || got.Outcome != knowledgeOutcomeError {
				if name != "trailing" {
					t.Errorf("decoded = %+v, want the fail-closed fallback", got)
				}
			}
		})
	}
	good := decodeKnowledgeJournal([]byte(`{"block":"b","outcome":"ok"}`), &fallback)
	if good.Block != "b" || good.ChunkIDs == nil {
		t.Errorf("good = %+v", good)
	}
}

func TestKnowledgeModeIsReportedInTheEventAndSurvivesAReplay(t *testing.T) {
	embedder := &countingEmbedder{vector: []float32{1, 0, 0, 0}}
	view := &fakeView{size: 1, hits: []Hit{{ChunkID: "kbc_sem", Collection: "support", Label: "faq.md", Heading: "Refunds", Ordinal: 1, Content: contextTestText}}}
	settings := &EmbeddingSettings{Fingerprint: "fp", Destination: "api.example.com", Model: "m", Dimensions: 4, BatchSize: 96, MaxMemory: 1 << 20}

	cases := map[string]struct {
		opts     []RetrieverOption
		wantMode string
	}{
		ModeLexical: {wantMode: ModeLexical},
		ModeHybrid: {
			opts: []RetrieverOption{WithVectors(&VectorConfig{
				Index:    &fakeVectorIndex{view: view},
				Embedder: embedder,
				Settings: settings,
			})},
			wantMode: ModeHybrid,
		},
		ModeLexicalFallback: {
			opts: []RetrieverOption{WithVectors(&VectorConfig{
				Index:    &fakeVectorIndex{view: view},
				Embedder: &countingEmbedder{err: errors.New("provider down")},
				Settings: settings,
			})},
			wantMode: ModeLexicalFallback,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			index := &contextTestIndex{hits: sampleContextHits()}
			provider := newTestContextProvider(t, index, tc.opts...)
			inv := &contextJournalInvocation{}
			req := contextTurnRequest("support")
			firstBlocks, err := provider.TurnContext(durable.WithTurnScope(t.Context(), durable.NewTurnScope(inv)), req)
			if err != nil {
				t.Fatalf("first TurnContext: %v", err)
			}
			firstEv := decodeContextRetrieved(t, &firstBlocks[0].Events[0])
			if firstEv.Mode != tc.wantMode || firstEv.Outcome != knowledgeOutcomeOK {
				t.Errorf("event mode %q outcome %q, want %q and ok", firstEv.Mode, firstEv.Outcome, tc.wantMode)
			}
			index.mu.Lock()
			index.err = errors.New("changed")
			index.mu.Unlock()
			replayedBlocks, err := provider.TurnContext(durable.WithTurnScope(t.Context(), durable.NewTurnScope(inv)), req)
			if err != nil {
				t.Fatalf("replayed TurnContext: %v", err)
			}
			replayedEv := decodeContextRetrieved(t, &replayedBlocks[0].Events[0])
			if replayedEv.Mode != tc.wantMode {
				t.Errorf("replayed mode %q, want %q", replayedEv.Mode, tc.wantMode)
			}
		})
	}
}

func TestKnowledgeModeIsOmittedWhenRetrievalFailed(t *testing.T) {
	index := &contextTestIndex{err: errors.New("store busy")}
	provider := newTestContextProvider(t, index)
	blocks, err := provider.TurnContext(t.Context(), contextTurnRequest("support"))
	if err != nil {
		t.Fatalf("TurnContext: %v", err)
	}
	if len(blocks) != 1 || len(blocks[0].Events) != 1 {
		t.Fatalf("blocks = %+v", blocks)
	}
	if got := decodeContextRetrieved(t, &blocks[0].Events[0]); got.Mode != "" || got.Outcome != knowledgeOutcomeError {
		t.Errorf("payload = %+v, want no mode on a failed retrieval", got)
	}
}

func TestKnowledgeJournalWrittenBeforeModeExistedStillDecodes(t *testing.T) {
	fallback := knowledgeJournal{ChunkIDs: []string{}, Outcome: knowledgeOutcomeError}
	got := decodeKnowledgeJournal([]byte(`{"block":"b","chunk_ids":["c"],"counts":{"retrieved":1,"rendered":1},"duration_ms":3,"outcome":"ok"}`), &fallback)
	if got.Block != "b" || got.Outcome != knowledgeOutcomeOK || got.Mode != "" {
		t.Errorf("decoded = %+v", got)
	}
}
