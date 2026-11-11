package runtimeadk

import (
	"context"
	"encoding/json"
	"iter"
	"slices"
	"sync"
	"testing"

	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/anggasct/aura/internal/platform/durable"
	"github.com/anggasct/aura/internal/store"
)

func journaledRequest() *adkmodel.LLMRequest {
	return &adkmodel.LLMRequest{Contents: []*genai.Content{{Role: genai.RoleUser, Parts: []*genai.Part{{Text: "hi"}}}}}
}

func scopedContext(t *testing.T, inv durable.Invocation) context.Context {
	t.Helper()
	return durable.WithTurnScope(t.Context(), durable.NewTurnScope(inv))
}

func journaledKinds(ctx context.Context, t *testing.T, model adkmodel.LLM, stream bool) (partials, finals int, err error) {
	t.Helper()
	for response, genErr := range model.GenerateContent(ctx, journaledRequest(), stream) {
		if genErr != nil {
			return partials, finals, genErr
		}
		if response.Partial {
			partials++
		} else {
			finals++
		}
	}
	return partials, finals, nil
}

func journalSnapshot(t *testing.T, inv *journalInvocation) map[string][]*adkmodel.LLMResponse {
	t.Helper()
	inv.mu.Lock()
	defer inv.mu.Unlock()
	out := map[string][]*adkmodel.LLMResponse{}
	for key, raw := range inv.journal {
		var chunks []*adkmodel.LLMResponse
		if err := json.Unmarshal(raw, &chunks); err != nil {
			continue
		}
		out[key] = chunks
	}
	return out
}

func TestJournaledModelYieldsPartialsLiveAndJournalsOnlyTheFinalResponse(t *testing.T) {
	inner := &streamingADKModel{chunks: []string{"par", "tial"}, finalTokens: 4}
	inv := &journalInvocation{}

	partials, finals, err := journaledKinds(scopedContext(t, inv), t, journalModel(inner), true)

	if err != nil {
		t.Fatalf("GenerateContent: %v", err)
	}
	if partials != 2 || finals != 1 {
		t.Errorf("responses = %d partial, %d final, want 2 and 1", partials, finals)
	}
	if !slices.Equal(inner.streams, []bool{true}) {
		t.Errorf("provider stream flags = %v, want [true]", inner.streams)
	}
	journal := journalSnapshot(t, inv)
	if len(journal) != 1 {
		t.Fatalf("journal entries = %d, want 1", len(journal))
	}
	for key, chunks := range journal {
		if len(chunks) != 1 || chunks[0].Partial || chunks[0].Content.Parts[0].Text != "partial" {
			t.Errorf("journal entry %q = %+v, want only the final response", key, chunks)
		}
	}
}

func TestJournaledModelReplayYieldsTheFinalResponseWithoutCallingTheProvider(t *testing.T) {
	inner := &streamingADKModel{chunks: []string{"par", "tial"}, finalTokens: 4}
	inv := &journalInvocation{}
	if _, _, err := journaledKinds(scopedContext(t, inv), t, journalModel(inner), true); err != nil {
		t.Fatalf("first run: %v", err)
	}

	partials, finals, err := journaledKinds(scopedContext(t, inv), t, journalModel(inner), true)

	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if partials != 0 || finals != 1 {
		t.Errorf("replayed responses = %d partial, %d final, want 0 and 1", partials, finals)
	}
	if len(inner.streams) != 1 {
		t.Errorf("provider calls = %d, want the replay served from the journal", len(inner.streams))
	}
}

func TestJournaledModelCallsTheProviderWithTheCallersStreamFlag(t *testing.T) {
	tests := []struct {
		name         string
		stream       bool
		wantPartials int
	}{
		{name: "streaming", stream: true, wantPartials: 2},
		{name: "not streaming", stream: false, wantPartials: 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			inner := &streamingADKModel{chunks: []string{"par", "tial"}, finalTokens: 4}

			partials, finals, err := journaledKinds(scopedContext(t, &journalInvocation{}), t, journalModel(inner), tc.stream)

			if err != nil {
				t.Fatalf("GenerateContent: %v", err)
			}
			if partials != tc.wantPartials || finals != 1 {
				t.Errorf("responses = %d partial, %d final, want %d and 1", partials, finals, tc.wantPartials)
			}
			if !slices.Equal(inner.streams, []bool{tc.stream}) {
				t.Errorf("provider stream flags = %v, want [%v]", inner.streams, tc.stream)
			}
		})
	}
}

func TestJournaledModelStopsCleanlyWhenTheConsumerStops(t *testing.T) {
	inner := &streamingADKModel{chunks: []string{"a", "b", "c", "d"}, finalTokens: 4}
	inv := &journalInvocation{}

	var seen int
	for response, err := range journalModel(inner).GenerateContent(scopedContext(t, inv), journaledRequest(), true) {
		if err != nil {
			t.Fatalf("consumer saw an error after stopping: %v", err)
		}
		if response.Partial {
			seen++
			break
		}
	}

	if seen != 1 {
		t.Errorf("partials seen = %d, want 1", seen)
	}
	if inner.partialsSent != 1 {
		t.Errorf("provider kept streaming after the consumer stopped: %d partials produced", inner.partialsSent)
	}
	if journal := journalSnapshot(t, inv); len(journal) != 0 {
		t.Errorf("a stopped stream was journaled: %+v", journal)
	}
	if _, finals, err := journaledKinds(scopedContext(t, inv), t, journalModel(inner), true); err != nil || finals != 1 {
		t.Errorf("next call after a stop = %d finals, err %v, want a fresh provider call", finals, err)
	}
}

func TestDurableExecutorStreamsPartialsWithoutChangingTheJournal(t *testing.T) {
	run := func(t *testing.T, streamPartial bool, inv *journalInvocation, model *streamingADKModel) []store.RuntimeEvent {
		t.Helper()
		modelName, models := streamingModelSet(model)
		executor, db, _ := newADKTestExecutor(t, modelName, models, &fakeBroker{})
		mustCreateSession(t, db, "session-1")
		req := streamTurn("turn-1", streamPartial, 0)
		var events []store.RuntimeEvent
		for ev, err := range executor.Execute(scopedContext(t, inv), req) {
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			events = append(events, ev)
		}
		return events
	}
	keysOf := func(inv *journalInvocation) []string {
		inv.mu.Lock()
		defer inv.mu.Unlock()
		keys := make([]string, 0, len(inv.journal))
		for key := range inv.journal {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		return keys
	}

	plainModel := &streamingADKModel{chunks: []string{"par", "tial"}, finalTokens: 4}
	plainInv := &journalInvocation{}
	run(t, false, plainInv, plainModel)

	streamModel := &streamingADKModel{chunks: []string{"par", "tial"}, finalTokens: 4}
	streamInv := &journalInvocation{}
	events := run(t, true, streamInv, streamModel)

	partials, finals := partialTexts(t, events)
	if !slices.Equal(partials, []string{"par", "tial"}) || finals != 1 {
		t.Errorf("events = %q partial, %d final, want two partials and one final", partials, finals)
	}
	if !slices.Equal(streamModel.streams, []bool{true}) || !slices.Equal(plainModel.streams, []bool{false}) {
		t.Errorf("provider stream flags: streaming %v, plain %v", streamModel.streams, plainModel.streams)
	}
	if got, want := keysOf(streamInv), keysOf(plainInv); !slices.Equal(got, want) {
		t.Errorf("journal keys with partials = %v, without = %v, want the same operations", got, want)
	}

	replayed := run(t, true, streamInv, streamModel)

	if partials, finals := partialTexts(t, replayed); len(partials) != 0 || finals != 1 {
		t.Errorf("replayed events = %q partial, %d final, want no partials", partials, finals)
	}
	if len(streamModel.streams) != 1 {
		t.Errorf("provider calls after replay = %d, want 1", len(streamModel.streams))
	}
}

type functionCallModel struct {
	mu    sync.Mutex
	calls int
}

func (m *functionCallModel) Name() string { return "function-call-model" }

func (m *functionCallModel) GenerateContent(context.Context, *adkmodel.LLMRequest, bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		m.mu.Lock()
		m.calls++
		m.mu.Unlock()
		yield(&adkmodel.LLMResponse{
			Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{
				{FunctionCall: &genai.FunctionCall{Name: "lookup", Args: map[string]any{"q": "a"}}},
				{FunctionCall: &genai.FunctionCall{ID: "call-7", Name: "lookup", Args: map[string]any{"q": "b"}}},
				{FunctionCall: &genai.FunctionCall{Name: "lookup", Args: map[string]any{"q": "c"}}},
			}},
			TurnComplete: true,
		}, nil)
	}
}

func (m *functionCallModel) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

func yieldedFunctionCallIDs(ctx context.Context, t *testing.T, model adkmodel.LLM) []string {
	t.Helper()
	var ids []string
	for response, err := range model.GenerateContent(ctx, journaledRequest(), false) {
		if err != nil {
			t.Fatalf("GenerateContent: %v", err)
		}
		for _, part := range response.Content.Parts {
			if part.FunctionCall != nil {
				ids = append(ids, part.FunctionCall.ID)
			}
		}
	}
	return ids
}

func TestJournaledModelGivesAnUnnamedFunctionCallAStableID(t *testing.T) {
	inner := &functionCallModel{}
	inv := &journalInvocation{}
	want := []string{"adk-turn-1-op-1-0", "call-7", "adk-turn-1-op-1-2"}

	first := yieldedFunctionCallIDs(withTurnID(scopedContext(t, inv), "turn-1"), t, journalModel(inner))
	second := yieldedFunctionCallIDs(withTurnID(scopedContext(t, inv), "turn-1"), t, journalModel(inner))

	if !slices.Equal(first, want) {
		t.Errorf("function call ids = %v, want %v", first, want)
	}
	if !slices.Equal(second, want) {
		t.Errorf("function call ids on the second call = %v, want %v", second, want)
	}
	if calls := inner.callCount(); calls != 1 {
		t.Errorf("model calls = %d, want the second call served from the journal", calls)
	}
	recorded := journalSnapshot(t, inv)["op-1"]
	if len(recorded) != 1 || recorded[0].Content.Parts[0].FunctionCall.ID != want[0] {
		t.Errorf("recorded response = %+v, want it to carry the id %s", recorded, want[0])
	}

	unscoped := yieldedFunctionCallIDs(withTurnID(t.Context(), "turn-1"), t, journalModel(inner))

	if !slices.Equal(unscoped, []string{"", "call-7", ""}) {
		t.Errorf("function call ids outside a turn scope = %v, want the unnamed calls left empty", unscoped)
	}
	if calls := inner.callCount(); calls != 2 {
		t.Errorf("model calls = %d, want the call outside a turn scope to reach the model", calls)
	}
}
