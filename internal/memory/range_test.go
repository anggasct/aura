package memory

import (
	"context"
	"testing"
	"time"
)

func rangeServiceWithStore(store *fakeDocumentStore) *Service {
	service, err := NewService(store, &fakeSecrets{known: map[string]bool{}}, &Config{RangeThreshold: 6, RangeLiveWindow: 2})
	if err != nil {
		panic(err)
	}
	return service
}

func seedRangeSession(t *testing.T, service *Service, session string, count int) {
	t.Helper()
	now := time.Now().UTC()
	for i := 1; i <= count; i++ {
		content := "archive backup old note"
		if i > 4 {
			content = "live backup recent note"
		}
		event := batchEvent(t, session, uint64(i), "user", "adk_event", content+" "+string(rune('a'+i)))
		event.CreatedAt = now
		if _, _, err := service.ProjectEvent(t.Context(), "owner-1", event); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}
}

func TestRangeSummaryCreatesOneLinkedDoc(t *testing.T) {
	store := newFakeDocumentStore()
	service := rangeServiceWithStore(store)
	seedRangeSession(t, service, "sess-range", 6)
	summarizer := &fakeSummarizer{text: "archive backup digest"}
	result, summarized, err := service.SummarizeClosedRange(t.Context(), "owner-1", "sess-range", capableModel(), summarizer)
	if err != nil {
		t.Fatalf("SummarizeClosedRange(): %v", err)
	}
	if !summarized || result.Summary == "" {
		t.Fatalf("summarized = %v, result = %+v", summarized, result)
	}
	if summarizer.calls != 1 {
		t.Fatalf("calls = %d, want 1", summarizer.calls)
	}
	for _, document := range summarizer.last.Documents {
		if document.PromptVersion != "" {
			t.Errorf("summary input includes derived doc %+v, want raw only", document)
		}
	}
	summaries, err := store.ListSessionDocuments(t.Context(), "owner-1", "sess-range", KindSummary, 10)
	if err != nil {
		t.Fatalf("list summaries: %v", err)
	}
	if len(summaries) != 1 {
		t.Fatalf("summaries = %d, want 1", len(summaries))
	}
	summary := summaries[0]
	if summary.SessionID != "sess-range" || summary.FromSequence != 1 || summary.ToSequence != 4 {
		t.Errorf("range = %s %d-%d, want sess-range 1-4", summary.SessionID, summary.FromSequence, summary.ToSequence)
	}
	if summary.PromptVersion == "" || summary.ExpiresAt == nil {
		t.Errorf("provenance missing: %+v", summary)
	}
	again, resummarized, err := service.SummarizeClosedRange(t.Context(), "owner-1", "sess-range", capableModel(), summarizer)
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if resummarized || again.Summary != "" {
		t.Errorf("second pass summarized = %v, want idempotent skip", resummarized)
	}
	if summarizer.calls != 1 {
		t.Errorf("calls = %d, want still 1", summarizer.calls)
	}
	summaries, err = store.ListSessionDocuments(t.Context(), "owner-1", "sess-range", KindSummary, 10)
	if err != nil || len(summaries) != 1 {
		t.Errorf("summaries after replay = %d, %v; want still 1", len(summaries), err)
	}
}

func TestRangeSummaryBelowThresholdStaysRaw(t *testing.T) {
	store := newFakeDocumentStore()
	service := rangeServiceWithStore(store)
	seedRangeSession(t, service, "sess-tiny", 2)
	summarizer := &fakeSummarizer{text: "archive backup digest"}
	_, summarized, err := service.SummarizeClosedRange(t.Context(), "owner-1", "sess-tiny", capableModel(), summarizer)
	if err != nil {
		t.Fatalf("SummarizeClosedRange(): %v", err)
	}
	if summarized || summarizer.calls != 0 {
		t.Errorf("summarized = %v, calls = %d; want raw-only", summarized, summarizer.calls)
	}
	summaries, err := store.ListSessionDocuments(t.Context(), "owner-1", "sess-tiny", KindSummary, 10)
	if err != nil || len(summaries) != 0 {
		t.Errorf("summaries = %d, %v; want none", len(summaries), err)
	}
}

func TestRangeRecallOrdersSummaryFirstForOldLiveRawFirst(t *testing.T) {
	store := newFakeDocumentStore()
	service := rangeServiceWithStore(store)
	seedRangeSession(t, service, "sess-range", 6)
	summarizer := &fakeSummarizer{text: "archive backup digest"}
	if _, _, err := service.SummarizeClosedRange(t.Context(), "owner-1", "sess-range", capableModel(), summarizer); err != nil {
		t.Fatalf("summarize: %v", err)
	}
	old, err := service.Recall(t.Context(), &RecallRequest{OwnerID: "owner-1", SessionID: "sess-range", Query: "backup archive"})
	if err != nil {
		t.Fatalf("Recall(): %v", err)
	}
	if len(old) == 0 || old[0].PromptVersion == "" {
		t.Fatalf("old-range recall first = %+v, want summary first", old)
	}
	for _, document := range old[1:] {
		if document.PromptVersion != "" {
			t.Errorf("multiple summaries in old recall: %+v", old)
			break
		}
	}
	all, err := service.Recall(t.Context(), &RecallRequest{OwnerID: "owner-1", SessionID: "sess-range", Query: "backup"})
	if err != nil {
		t.Fatalf("Recall(): %v", err)
	}
	if len(all) != 7 {
		t.Fatalf("documents = %d, want 7 (2 live + 1 summary + 4 old)", len(all))
	}
	if all[0].PromptVersion != "" || all[1].PromptVersion != "" {
		t.Errorf("first two must be live raw, got %+v", all[:2])
	}
	if all[0].FromSequence <= 4 || all[1].FromSequence <= 4 {
		t.Errorf("live docs must be seq > 4, got %+v", all[:2])
	}
	if all[2].PromptVersion == "" {
		t.Errorf("third must be summary, got %+v", all[2])
	}
	for _, document := range all[3:] {
		if document.PromptVersion != "" {
			t.Errorf("old tail holds summary: %+v", all)
			break
		}
		if document.FromSequence > 4 {
			t.Errorf("old tail holds live seq: %+v", document)
		}
	}
}

func TestRangeReopenMarksStaleUntilRebuild(t *testing.T) {
	store := newFakeDocumentStore()
	service := rangeServiceWithStore(store)
	seedRangeSession(t, service, "sess-range", 6)
	summarizer := &fakeSummarizer{text: "archive backup digest"}
	if _, _, err := service.SummarizeClosedRange(t.Context(), "owner-1", "sess-range", capableModel(), summarizer); err != nil {
		t.Fatalf("summarize: %v", err)
	}
	event := batchEvent(t, "sess-range", 7, "user", "adk_event", "live backup recent note g")
	if _, _, err := service.ProjectEvent(t.Context(), "owner-1", event); err != nil {
		t.Fatalf("reopen: %v", err)
	}
	summaries, err := store.ListSessionDocuments(t.Context(), "owner-1", "sess-range", KindSummary, 10)
	if err != nil || len(summaries) != 0 {
		t.Fatalf("summaries after reopen = %d, %v; want stale-removed", len(summaries), err)
	}
	fallback, err := service.Recall(t.Context(), &RecallRequest{OwnerID: "owner-1", SessionID: "sess-range", Query: "backup archive"})
	if err != nil {
		t.Fatalf("Recall(): %v", err)
	}
	for _, document := range fallback {
		if document.PromptVersion != "" {
			t.Fatalf("stale summary served: %+v", fallback)
		}
	}
	if _, resummarized, err := service.SummarizeClosedRange(t.Context(), "owner-1", "sess-range", capableModel(), summarizer); err != nil {
		t.Fatalf("rebuild: %v", err)
	} else if !resummarized {
		t.Fatal("rebuild skipped")
	}
	summaries, err = store.ListSessionDocuments(t.Context(), "owner-1", "sess-range", KindSummary, 10)
	if err != nil || len(summaries) != 1 {
		t.Fatalf("summaries after rebuild = %d, %v; want 1", len(summaries), err)
	}
	if summaries[0].FromSequence != 1 || summaries[0].ToSequence != 5 {
		t.Errorf("rebuilt range = %d-%d, want 1-5", summaries[0].FromSequence, summaries[0].ToSequence)
	}
}

func TestRangeCapabilitySkipLeavesZeroDocs(t *testing.T) {
	store := newFakeDocumentStore()
	service := rangeServiceWithStore(store)
	seedRangeSession(t, service, "sess-range", 6)
	incapable := capableModel()
	incapable.StructuredOutput = false
	summarizer := &fakeSummarizer{text: "unused"}
	if _, _, err := service.SummarizeClosedRange(t.Context(), "owner-1", "sess-range", incapable, summarizer); err == nil {
		t.Fatal("incapable model accepted")
	} else if code, ok := CodeOf(err); !ok || code != ErrorCodeModelCapabilityUnsupported {
		t.Fatalf("code = %v, %v", code, ok)
	}
	if summarizer.calls != 0 {
		t.Errorf("model called %d times, want 0", summarizer.calls)
	}
	summaries, err := store.ListSessionDocuments(t.Context(), "owner-1", "sess-range", KindSummary, 10)
	if err != nil || len(summaries) != 0 {
		t.Errorf("summaries = %d, %v; want zero", len(summaries), err)
	}
}

func TestRangeValidation(t *testing.T) {
	store := newFakeDocumentStore()
	service := rangeServiceWithStore(store)
	summarizer := &fakeSummarizer{text: "x"}
	if _, _, err := service.SummarizeClosedRange(t.Context(), "", "sess-1", capableModel(), summarizer); err == nil {
		t.Error("empty owner accepted")
	}
	if _, _, err := service.SummarizeClosedRange(t.Context(), "owner-1", "", capableModel(), summarizer); err == nil {
		t.Error("empty session accepted")
	}
	if _, _, err := service.SummarizeClosedRange(t.Context(), "owner-1", "sess-1", nil, summarizer); err == nil {
		t.Error("nil model accepted")
	}
	if _, _, err := service.SummarizeClosedRange(t.Context(), "owner-1", "sess-1", capableModel(), nil); err == nil {
		t.Error("nil summarizer accepted")
	}
	var nilCtx context.Context
	if _, _, err := service.SummarizeClosedRange(nilCtx, "owner-1", "sess-1", capableModel(), summarizer); err == nil {
		t.Error("nil context accepted")
	}
	var nilService *Service
	if _, _, err := nilService.SummarizeClosedRange(t.Context(), "owner-1", "sess-1", capableModel(), summarizer); err == nil {
		t.Error("nil service accepted")
	}
	for _, config := range []Config{{RangeThreshold: -1}, {RangeThreshold: maxRangeThreshold + 1}, {RangeLiveWindow: -1}, {RangeLiveWindow: maxRangeLiveWindow + 1}} {
		if _, err := NewService(newFakeDocumentStore(), nil, &config); err == nil {
			t.Errorf("invalid range config accepted: %+v", config)
		} else if code, ok := CodeOf(err); !ok || code != ErrorCodeBudgetInvalid {
			t.Errorf("code = %v, %v; want budget invalid", code, ok)
		}
	}
}
