package memory

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/approval"
)

type fakeSummarizer struct {
	text  string
	err   error
	calls int
	last  *SummaryPrompt
}

func (f *fakeSummarizer) Summarize(_ context.Context, prompt *SummaryPrompt) (string, error) {
	f.calls++
	f.last = prompt
	if f.err != nil {
		return "", f.err
	}
	return f.text, nil
}

func capableModel() *SummaryModel {
	return &SummaryModel{
		Protocol:         "anthropic_messages",
		Name:             "claude-sonnet-4-20250514",
		Tokenizer:        "claude",
		ContextTokens:    200000,
		StructuredOutput: true,
	}
}

func seedRecallDocuments(t *testing.T, service *Service, owner, session string, contents ...string) []RecallDocument {
	t.Helper()
	now := time.Now().UTC()
	for i, content := range contents {
		event := &Event{
			ID: "ev-sum-" + content, SessionID: session, Sequence: uint64(i + 1),
			Author: "user", Kind: "adk_event",
			Payload: adkPayload(t, "user", content, false), CreatedAt: now,
		}
		if _, _, err := service.ProjectEvent(t.Context(), owner, event); err != nil {
			t.Fatalf("ProjectEvent(): %v", err)
		}
	}
	documents, err := service.Recall(t.Context(), &RecallRequest{
		OwnerID: owner, SessionID: session, Query: "backup",
	})
	if err != nil {
		t.Fatalf("Recall(): %v", err)
	}
	if len(documents) != len(contents) {
		t.Fatalf("documents = %d, want %d", len(documents), len(contents))
	}
	return documents
}

func TestSummarizePersistsProvenanceBearingSummary(t *testing.T) {
	store := newFakeDocumentStore()
	service := testServiceWithStore(store)
	documents := seedRecallDocuments(t, service, "owner-1", "sess-1", "nightly backup finished", "backup failed with errors")

	summarizer := &fakeSummarizer{text: "Two backup notes: one clean run, one failure."}
	result, err := service.Summarize(t.Context(), "owner-1", "sess-1", "backup status", documents, capableModel(), summarizer)
	if err != nil {
		t.Fatalf("Summarize(): %v", err)
	}
	if result.Summary == "" {
		t.Error("summary missing")
	}
	if result.Trust != approval.TrustDerivedUntrusted {
		t.Errorf("trust = %q, want derived_untrusted", result.Trust)
	}
	if len(result.Provenance) != len(documents) {
		t.Fatalf("provenance refs = %d, want %d", len(result.Provenance), len(documents))
	}
	for i, ref := range result.Provenance {
		if ref.DocumentID != documents[i].ID || ref.FromSequence != documents[i].FromSequence || ref.ToSequence != documents[i].ToSequence {
			t.Errorf("provenance[%d] = %+v, want ref to %+v", i, ref, documents[i])
		}
	}
	if summarizer.last == nil || summarizer.last.Version != defaultSummaryPromptVersion {
		t.Errorf("prompt version = %+v, want %q", summarizer.last, defaultSummaryPromptVersion)
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	var persisted *StoredDocument
	for id := range store.records {
		record := store.records[id]
		if record.Kind == KindSummary {
			persisted = &record
		}
	}
	if persisted == nil {
		t.Fatal("summary document not persisted")
	}
	if persisted.TrustLabel != string(approval.TrustDerivedUntrusted) {
		t.Errorf("trust = %q", persisted.TrustLabel)
	}
	if persisted.PromptVersion != defaultSummaryPromptVersion {
		t.Errorf("prompt version = %q", persisted.PromptVersion)
	}
	if persisted.ModelProtocol != "anthropic_messages" || persisted.ModelName != "claude-sonnet-4-20250514" {
		t.Errorf("model provenance = %q/%q", persisted.ModelProtocol, persisted.ModelName)
	}
	if persisted.FromSequence != 1 || persisted.ToSequence != 2 {
		t.Errorf("source range = %d-%d, want 1-2", persisted.FromSequence, persisted.ToSequence)
	}
	if persisted.ExpiresAt == nil || time.Until(*persisted.ExpiresAt) <= 0 {
		t.Errorf("expiry = %v, want future TTL", persisted.ExpiresAt)
	}
}

func TestSummarizeCapabilityGateFailsBeforeModelCall(t *testing.T) {
	service := testService()
	documents := seedRecallDocuments(t, service, "owner-1", "sess-1", "nightly backup finished")

	noStructured := capableModel()
	noStructured.StructuredOutput = false
	summarizer := &fakeSummarizer{text: "unused"}
	if _, err := service.Summarize(t.Context(), "owner-1", "sess-1", "backup", documents, noStructured, summarizer); err == nil {
		t.Error("model without structured output accepted")
	} else if code, ok := CodeOf(err); !ok || code != ErrorCodeModelCapabilityUnsupported {
		t.Errorf("code = %v, %v", code, ok)
	}
	if summarizer.calls != 0 {
		t.Errorf("model called %d times, want 0", summarizer.calls)
	}

	tiny := capableModel()
	tiny.ContextTokens = 8
	if _, err := service.Summarize(t.Context(), "owner-1", "sess-1", "backup", documents, tiny, summarizer); err == nil {
		t.Error("overflowing context accepted")
	} else if code, ok := CodeOf(err); !ok || code != ErrorCodeModelCapabilityUnsupported {
		t.Errorf("code = %v, %v", code, ok)
	}
	if summarizer.calls != 0 {
		t.Errorf("model called %d times, want 0", summarizer.calls)
	}
}

func TestSummarizeUnknownCapabilityUsesConservativeLimits(t *testing.T) {
	service := testService()
	documents := seedRecallDocuments(t, service, "owner-1", "sess-1", "nightly backup finished")

	unknown := capableModel()
	unknown.Tokenizer = ""
	unknown.ContextTokens = 0
	summarizer := &fakeSummarizer{text: "conservative summary"}
	result, err := service.Summarize(t.Context(), "owner-1", "sess-1", "backup", documents, unknown, summarizer)
	if err != nil {
		t.Fatalf("Summarize(): %v", err)
	}
	if result.Summary == "" || summarizer.calls != 1 {
		t.Errorf("result = %+v, calls = %d", result, summarizer.calls)
	}

	tight := capableModel()
	tight.Tokenizer = ""
	tight.ContextTokens = estimateTokens("backup") + estimateTokens("nightly backup finished") + service.tokens + 4
	if _, err := service.Summarize(t.Context(), "owner-1", "sess-1", "backup", documents, tight, &fakeSummarizer{text: "x"}); err == nil {
		t.Error("unknown-tokenizer margin not applied")
	} else if code, ok := CodeOf(err); !ok || code != ErrorCodeModelCapabilityUnsupported {
		t.Errorf("code = %v, %v", code, ok)
	}
}

func TestSummarizeValidation(t *testing.T) {
	service := testService()
	documents := seedRecallDocuments(t, service, "owner-1", "sess-1", "nightly backup finished")
	summarizer := &fakeSummarizer{text: "summary"}

	if _, err := service.Summarize(t.Context(), "", "sess-1", "q", documents, capableModel(), summarizer); err == nil {
		t.Error("empty owner accepted")
	}
	if _, err := service.Summarize(t.Context(), "owner-1", "sess-1", "q", nil, capableModel(), summarizer); err == nil {
		t.Error("empty documents accepted")
	}
	if _, err := service.Summarize(t.Context(), "owner-1", "sess-1", "q", documents, capableModel(), nil); err == nil {
		t.Error("nil summarizer accepted")
	}
	if _, err := service.Summarize(t.Context(), "owner-1", "sess-1", "q", documents, nil, summarizer); err == nil {
		t.Error("nil model accepted")
	}
	anonymous := capableModel()
	anonymous.Name = ""
	if _, err := service.Summarize(t.Context(), "owner-1", "sess-1", "q", documents, anonymous, summarizer); err == nil {
		t.Error("anonymous model accepted")
	}
	if _, err := service.Summarize(t.Context(), "owner-1", "sess-1", "q", documents, capableModel(), &fakeSummarizer{text: "  "}); err == nil {
		t.Error("empty summary accepted")
	} else if code, ok := CodeOf(err); !ok || code != ErrorCodeUnavailable {
		t.Errorf("code = %v, %v", code, ok)
	}
}

func TestRecallWithSummaryDegradesToNoRecall(t *testing.T) {
	service := testService()
	seedRecallDocuments(t, service, "owner-1", "sess-1", "nightly backup finished")

	plain, err := service.RecallWithSummary(t.Context(), &RecallRequest{
		OwnerID: "owner-1", SessionID: "sess-1", Query: "nightly backup finished",
	}, nil, nil)
	if err != nil {
		t.Fatalf("RecallWithSummary(): %v", err)
	}
	if len(plain.Documents) != 1 || plain.Summary != "" || len(plain.Provenance) != 1 {
		t.Errorf("plain recall = %+v", plain)
	}

	failing := &fakeSummarizer{err: errors.New("model exploded")}
	degraded, err := service.RecallWithSummary(t.Context(), &RecallRequest{
		OwnerID: "owner-1", SessionID: "sess-1", Query: "nightly backup finished",
	}, capableModel(), failing)
	if err == nil {
		t.Fatal("model failure swallowed")
	}
	if len(degraded.Documents) != 0 || degraded.Summary != "" || len(degraded.Provenance) != 0 {
		t.Errorf("partial context leaked: %+v", degraded)
	}

	broken := NewServiceWithStoreError()
	if _, err := broken.RecallWithSummary(t.Context(), &RecallRequest{
		OwnerID: "owner-1", SessionID: "sess-1", Query: "nightly backup finished",
	}, capableModel(), &fakeSummarizer{text: "x"}); err == nil {
		t.Fatal("projection failure swallowed")
	} else {
		empty, _ := broken.RecallWithSummary(t.Context(), &RecallRequest{
			OwnerID: "owner-1", SessionID: "sess-1", Query: "nightly backup finished",
		}, nil, nil)
		if len(empty.Documents) != 0 {
			t.Errorf("partial context leaked: %+v", empty)
		}
	}
}

func TestRecallSurfacesCorruptProjection(t *testing.T) {
	store := newFakeDocumentStore()
	store.records["mem_bad"] = StoredDocument{
		ID: "mem_bad", OwnerID: "owner-1", SessionID: "sess-1", Kind: KindEventText,
		FromSequence: -4, ToSequence: -4, Content: "nightly backup finished",
		TrustLabel: string(approval.TrustOwnerInput), CreatedAt: time.Now().UTC(),
	}
	service := testServiceWithStore(store)
	if _, err := service.Recall(t.Context(), &RecallRequest{
		OwnerID: "owner-1", SessionID: "sess-1", Query: "nightly backup finished",
	}); err == nil {
		t.Fatal("corrupt row accepted")
	} else if code, ok := CodeOf(err); !ok || code != ErrorCodeProjectionCorrupt {
		t.Errorf("code = %v, %v", code, ok)
	}
}

func TestSummaryRoundTripCarriesModelProvenance(t *testing.T) {
	store := newFakeDocumentStore()
	service := testServiceWithStore(store)
	documents := seedRecallDocuments(t, service, "owner-1", "sess-1", "nightly backup finished")
	if _, err := service.Summarize(t.Context(), "owner-1", "sess-1", "nightly backup finished", documents, capableModel(), &fakeSummarizer{text: "backup digest"}); err != nil {
		t.Fatalf("Summarize(): %v", err)
	}
	found, err := service.Recall(t.Context(), &RecallRequest{
		OwnerID: "owner-1", SessionID: "sess-1", Query: "backup digest",
	})
	if err != nil {
		t.Fatalf("Recall(): %v", err)
	}
	if len(found) != 1 {
		t.Fatalf("documents = %d, want 1", len(found))
	}
	if found[0].PromptVersion != defaultSummaryPromptVersion || found[0].ModelProtocol != "anthropic_messages" || found[0].ModelName != "claude-sonnet-4-20250514" {
		t.Errorf("provenance = %+v", found[0])
	}
	if found[0].Trust != approval.TrustDerivedUntrusted {
		t.Errorf("trust = %q", found[0].Trust)
	}
}
