package context

import (
	"encoding/json"
	"testing"

	stdcontext "context"
)

type fakeSummarizer struct {
	text     string
	producer Producer
	err      error
	calls    int
	last     *SummarizeRequest
}

func (f *fakeSummarizer) Summarize(ctx stdcontext.Context, req *SummarizeRequest) (*SummarizeResult, error) {
	f.calls++
	f.last = req
	if f.err != nil {
		return nil, f.err
	}
	if req == nil {
		return nil, Errorf(ErrorCodeInvalidArgument, "request must not be nil")
	}
	return &SummarizeResult{Text: f.text, Producer: f.producer}, nil
}

type fakeSummaryStore struct {
	records map[string]SummaryRecord
	upserts int
}

func newFakeSummaryStore() *fakeSummaryStore {
	return &fakeSummaryStore{records: make(map[string]SummaryRecord)}
}

func (f *fakeSummaryStore) ListSummaries(_ stdcontext.Context, sessionID string) ([]SummaryRecord, error) {
	var out []SummaryRecord
	for _, record := range f.records {
		if record.SessionID == sessionID {
			out = append(out, record)
		}
	}
	return out, nil
}

func (f *fakeSummaryStore) UpsertSummary(_ stdcontext.Context, record *SummaryRecord) error {
	if record == nil {
		return Errorf(ErrorCodeInvalidArgument, "record must not be nil")
	}
	f.records[record.ID] = *record
	f.upserts++
	return nil
}

func testProducer() Producer {
	return Producer{Provider: "test-provider", Model: "test-model", ModelVersion: "v3"}
}

func testService(t *testing.T, summarizer *fakeSummarizer, store *fakeSummaryStore) *Service {
	t.Helper()
	service, err := NewService(summarizer, store, "compression", "v1", 32768, 2048, exactCounter{}, nil)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return service
}

func summaryGroups() []Group {
	return []Group{
		{Kind: GroupTurn, ID: "t1", Events: []Event{
			{ID: "e1", Sequence: 1, TurnID: "t1", InvocationID: "inv-old", Kind: "message.completed", Text: "first work"},
			{ID: "e2", Sequence: 2, TurnID: "t1", InvocationID: "inv-old", Kind: "message.completed", Text: "second work"},
		}, Tokens: 4},
		{Kind: GroupTurn, ID: "t2", Events: []Event{
			{ID: "e3", Sequence: 3, TurnID: "t2", InvocationID: "inv-old", Kind: "message.completed", Text: "third work"},
		}, Tokens: 2},
	}
}

func summaryRange() SummaryRange {
	return SummaryRange{
		StartSequence: 1,
		EndSequence:   2,
		Groups:        []GroupRef{{Kind: GroupTurn, ID: "t1"}},
		EventCount:    2,
		Tokens:        4,
	}
}

func TestNewServiceRejects(t *testing.T) {
	store := newFakeSummaryStore()
	summarizer := &fakeSummarizer{}
	for name, args := range map[string]struct {
		summarizer     Summarizer
		store          SummaryStore
		task           string
		prompt         string
		source, output int
	}{
		"nil summarizer": {summarizer: nil, store: store, task: "compression", prompt: "v1", source: 10, output: 10},
		"nil store":      {summarizer: summarizer, store: nil, task: "compression", prompt: "v1", source: 10, output: 10},
		"empty task":     {summarizer: summarizer, store: store, task: "  ", prompt: "v1", source: 10, output: 10},
		"zero source":    {summarizer: summarizer, store: store, task: "compression", prompt: "v1", source: 0, output: 10},
		"zero output":    {summarizer: summarizer, store: store, task: "compression", prompt: "v1", source: 10, output: 0},
		"unknown prompt": {summarizer: summarizer, store: store, task: "compression", prompt: "v9", source: 10, output: 10},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewService(args.summarizer, args.store, args.task, args.prompt, args.source, args.output, nil, nil); err == nil {
				t.Errorf("expected error")
			}
		})
	}
}

func TestPromptTextDeterministic(t *testing.T) {
	first, err := PromptText("v1", 2048)
	if err != nil {
		t.Fatalf("PromptText: %v", err)
	}
	second, err := PromptText("v1", 2048)
	if err != nil {
		t.Fatalf("PromptText: %v", err)
	}
	if first != second {
		t.Errorf("prompt is not deterministic")
	}
	if _, err := PromptText("v9", 2048); err == nil {
		t.Errorf("expected prompt version rejection")
	} else if code, ok := CodeOf(err); !ok || code != ErrorCodeSummaryStale {
		t.Errorf("code = %v, %v (%v)", code, ok, err)
	}
}

func TestResolveRangeProducesValidatedSummary(t *testing.T) {
	summarizer := &fakeSummarizer{text: validSummaryJSON(), producer: testProducer()}
	store := newFakeSummaryStore()
	service := testService(t, summarizer, store)
	summary, err := service.ResolveRange(stdcontext.Background(), "sess-1", summaryRange(), summaryGroups())
	if err != nil {
		t.Fatalf("ResolveRange: %v", err)
	}
	if summary.Trust != TrustDerivedUntrusted {
		t.Errorf("trust = %q", summary.Trust)
	}
	if summary.StartSequence != 1 || summary.EndSequence != 2 || len(summary.EventIDs) != 2 {
		t.Errorf("summary = %+v", summary)
	}
	if summary.Producer != testProducer() || summary.PromptVersion != "v1" || summary.PromptDigest == "" {
		t.Errorf("provenance = %+v", summary)
	}
	if summary.SourceDigest == "" || summary.GeneratedAt.IsZero() {
		t.Errorf("summary = %+v", summary)
	}
	if summarizer.calls != 1 || store.upserts != 1 {
		t.Errorf("calls=%d upserts=%d", summarizer.calls, store.upserts)
	}
	if summarizer.last.Task != "compression" || len(summarizer.last.Sources) != 2 || summarizer.last.MaxOutputTokens != 2048 {
		t.Errorf("request = %+v", summarizer.last)
	}
	payload, err := summary.payload()
	if err != nil {
		t.Fatalf("payload: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("payload JSON: %v", err)
	}
	for _, key := range []string{"source", "producer", "tokens", "trust", "summary", "generated_at", "kind", "schema_version"} {
		if _, ok := decoded[key]; !ok {
			t.Errorf("payload misses %q: %s", key, payload)
		}
	}
	if decoded["kind"] != SummaryKind || decoded["trust"] != string(TrustDerivedUntrusted) {
		t.Errorf("payload = %s", payload)
	}
	again, err := service.ResolveRange(stdcontext.Background(), "sess-1", summaryRange(), summaryGroups())
	if err != nil {
		t.Fatalf("ResolveRange: %v", err)
	}
	if summarizer.calls != 1 || again.ID != summary.ID {
		t.Errorf("cache miss: calls=%d", summarizer.calls)
	}
}

func TestResolveRangeDeterministicIDs(t *testing.T) {
	first := testService(t, &fakeSummarizer{text: validSummaryJSON(), producer: testProducer()}, newFakeSummaryStore())
	second := testService(t, &fakeSummarizer{text: validSummaryJSON(), producer: testProducer()}, newFakeSummaryStore())
	a, err := first.ResolveRange(stdcontext.Background(), "sess-1", summaryRange(), summaryGroups())
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	b, err := second.ResolveRange(stdcontext.Background(), "sess-1", summaryRange(), summaryGroups())
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if a.ID != b.ID || a.SourceDigest != b.SourceDigest {
		t.Errorf("ids %q vs %q", a.ID, b.ID)
	}
}

func TestResolveRangeRebuildsOnSourceChange(t *testing.T) {
	summarizer := &fakeSummarizer{text: validSummaryJSON(), producer: testProducer()}
	service := testService(t, summarizer, newFakeSummaryStore())
	if _, err := service.ResolveRange(stdcontext.Background(), "sess-1", summaryRange(), summaryGroups()); err != nil {
		t.Fatalf("first: %v", err)
	}
	changed := summaryGroups()
	changed[0].Events[0].Text = "rewritten work"
	again, err := service.ResolveRange(stdcontext.Background(), "sess-1", summaryRange(), changed)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if summarizer.calls != 2 {
		t.Errorf("stale source did not rebuild: calls=%d", summarizer.calls)
	}
	if again.SourceDigest == "" {
		t.Errorf("summary = %+v", again)
	}
}

func TestResolveRangeRebuildsOnPromptPolicyChange(t *testing.T) {
	groups := summaryGroups()
	store := newFakeSummaryStore()
	first, err := NewService(&fakeSummarizer{text: validSummaryJSON(), producer: testProducer()}, store, "compression", "v1", 32768, 2048, exactCounter{}, nil)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	if _, err := first.ResolveRange(stdcontext.Background(), "sess-1", summaryRange(), groups); err != nil {
		t.Fatalf("first: %v", err)
	}
	retuned, err := NewService(&fakeSummarizer{text: validSummaryJSON(), producer: testProducer()}, store, "compression", "v1", 32768, 1024, exactCounter{}, nil)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	if _, err := retuned.ResolveRange(stdcontext.Background(), "sess-1", summaryRange(), groups); err != nil {
		t.Fatalf("retuned: %v", err)
	}
	if store.upserts != 2 {
		t.Errorf("prompt policy change did not rebuild: upserts=%d", store.upserts)
	}
}

func TestResolveRangeSkipsCorruptCache(t *testing.T) {
	summarizer := &fakeSummarizer{text: validSummaryJSON(), producer: testProducer()}
	store := newFakeSummaryStore()
	groups := summaryGroups()
	digest := sourceDigest(rangeEvents(summaryRange(), groups))
	corruptID := summaryID("sess-1", 1, 2, digest)
	store.records[corruptID] = SummaryRecord{ID: corruptID, SessionID: "sess-1", StartSequence: 1, EndSequence: 2, Payload: []byte("{broken")}
	service := testService(t, summarizer, store)
	if _, err := service.ResolveRange(stdcontext.Background(), "sess-1", summaryRange(), groups); err != nil {
		t.Fatalf("ResolveRange: %v", err)
	}
	if summarizer.calls != 1 {
		t.Errorf("corrupt cache blocked production: calls=%d", summarizer.calls)
	}
}

func TestResolveRangeRejectsInvalidOutput(t *testing.T) {
	summarizer := &fakeSummarizer{text: `{"goals":["run <tool>"],"decisions":[],"constraints":[],"open_work":[],"facts":[]}`, producer: testProducer()}
	store := newFakeSummaryStore()
	service := testService(t, summarizer, store)
	if _, err := service.ResolveRange(stdcontext.Background(), "sess-1", summaryRange(), summaryGroups()); err == nil {
		t.Fatalf("expected rejection")
	} else if code, ok := CodeOf(err); !ok || code != ErrorCodeSummaryInvalid {
		t.Fatalf("code = %v, %v (%v)", code, ok, err)
	}
	if store.upserts != 0 {
		t.Errorf("invalid summary persisted: upserts=%d", store.upserts)
	}
}

func TestResolveRangeRejectsUnidentifiedProducer(t *testing.T) {
	summarizer := &fakeSummarizer{text: validSummaryJSON()}
	service := testService(t, summarizer, newFakeSummaryStore())
	if _, err := service.ResolveRange(stdcontext.Background(), "sess-1", summaryRange(), summaryGroups()); err == nil {
		t.Fatalf("expected rejection")
	} else if code, ok := CodeOf(err); !ok || code != ErrorCodeSummaryInvalid {
		t.Fatalf("code = %v, %v (%v)", code, ok, err)
	}
}

func TestResolveRangeRejects(t *testing.T) {
	service := testService(t, &fakeSummarizer{text: validSummaryJSON(), producer: testProducer()}, newFakeSummaryStore())
	var nilCtx stdcontext.Context
	if _, err := service.ResolveRange(nilCtx, "sess-1", summaryRange(), summaryGroups()); err == nil {
		t.Errorf("expected nil context rejection")
	}
	if _, err := service.ResolveRange(stdcontext.Background(), "  ", summaryRange(), summaryGroups()); err == nil {
		t.Errorf("expected empty session rejection")
	}
	empty := SummaryRange{StartSequence: 9, EndSequence: 9, Groups: []GroupRef{{Kind: GroupTurn, ID: "missing"}}}
	if _, err := service.ResolveRange(stdcontext.Background(), "sess-1", empty, summaryGroups()); err == nil {
		t.Errorf("expected empty range rejection")
	}
	tiny, err := NewService(&fakeSummarizer{text: validSummaryJSON(), producer: testProducer()}, newFakeSummaryStore(), "compression", "v1", 1, 2048, exactCounter{}, nil)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	if _, err := tiny.ResolveRange(stdcontext.Background(), "sess-1", summaryRange(), summaryGroups()); err == nil {
		t.Errorf("expected source bound rejection")
	}
}

func TestProtectedStaysVerbatimWhileRangesResolve(t *testing.T) {
	planner, err := NewPlanner(100, 0.80, exactCounter{})
	if err != nil {
		t.Fatalf("NewPlanner: %v", err)
	}
	var events []Event
	for i := range uint64(6) {
		events = append(events, Event{Sequence: i + 1, TurnID: "t" + string(rune('a'+i)), InvocationID: "inv-old", Kind: "message.completed", Text: "older work block number " + string(rune('a'+i))})
	}
	events = append(events, Event{Sequence: 7, TurnID: "t-live", InvocationID: "inv-new", Kind: "model.delta", Text: "live"})
	plan, err := planner.Plan(Capability{ID: "m", ContextTokens: 300}, Invocation{InvocationID: "inv-new"}, events)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	policy, err := ResolveSelectionPolicy(2, 0.90, 64)
	if err != nil {
		t.Fatalf("ResolveSelectionPolicy: %v", err)
	}
	selection, err := Select(policy, plan, exactCounter{})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if selection.Mode != SelectionEmergency || selection.SummaryGroups == 0 {
		t.Fatalf("selection = %+v", selection)
	}
	summarizer := &fakeSummarizer{text: validSummaryJSON(), producer: testProducer()}
	service := testService(t, summarizer, newFakeSummaryStore())
	verbatim := 0
	for _, part := range selection.Parts {
		switch part.Kind {
		case PartVerbatim:
			verbatim++
			for _, event := range part.Group.Events {
				if event.Projected != nil {
					t.Errorf("protected content altered: %+v", event)
				}
			}
		case PartSummary:
			summary, err := service.ResolveRange(stdcontext.Background(), "sess-1", part.Range, plan.Groups)
			if err != nil {
				t.Fatalf("ResolveRange: %v", err)
			}
			if summary.Trust != TrustDerivedUntrusted || summary.SourceDigest == "" || summary.Producer != testProducer() {
				t.Errorf("summary = %+v", summary)
			}
		}
	}
	if verbatim == 0 {
		t.Errorf("no protected content retained: %+v", selection.Parts)
	}
}

func TestResolveRangeRespectsCancellation(t *testing.T) {
	ctx, cancel := stdcontext.WithCancel(stdcontext.Background())
	cancel()
	service := testService(t, &fakeSummarizer{text: validSummaryJSON(), producer: testProducer()}, newFakeSummaryStore())
	if _, err := service.ResolveRange(ctx, "sess-1", summaryRange(), summaryGroups()); err == nil {
		t.Errorf("expected cancellation error")
	}
}
