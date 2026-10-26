package cli

import (
	"context"
	"database/sql"
	"iter"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
	"time"

	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/anggasct/aura/internal/approval"
	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/memory"
	"github.com/anggasct/aura/internal/runtime"
	runtimeingress "github.com/anggasct/aura/internal/runtime/ingress"
	"github.com/anggasct/aura/internal/store"
	"github.com/anggasct/aura/internal/telemetry"
)

func newMemoryTestDB(t *testing.T) *sql.DB {
	t.Helper()
	ctx := context.Background()
	db, err := store.OpenDB(ctx, filepath.Join(t.TempDir(), "aura.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.Migrate(ctx, db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return db
}

func TestMemoryDocumentStoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	db := newMemoryTestDB(t)
	if _, err := db.ExecContext(ctx, `INSERT INTO session (id, owner_id, metadata_json, created_at, updated_at) VALUES (?,?,?,?,?)`,
		"sess-1", "owner-1", `{}`, "2026-09-13T00:00:00Z", "2026-09-13T00:00:00Z"); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	adapter := &memoryDocumentStore{store: store.NewMemoryStore(db)}
	now := time.Now().UTC().Truncate(time.Second)
	inserted, err := adapter.UpsertDocument(ctx, &memory.StoredDocument{
		ID: "mem_cli", OwnerID: "owner-1", SessionID: "sess-1", Kind: memory.KindEventText,
		FromSequence: 1, ToSequence: 1, Content: "the nightly backup finished",
		TrustLabel: "owner_input", PromptVersion: "", CreatedAt: now,
	})
	if err != nil || !inserted {
		t.Fatalf("UpsertDocument() = %v, %v", inserted, err)
	}
	hits, err := adapter.Search(ctx, &memory.StoredQuery{
		OwnerID: "owner-1", SessionID: "sess-1", Terms: "nightly backup", Limit: 10,
	})
	if err != nil {
		t.Fatalf("Search(): %v", err)
	}
	if len(hits) != 1 || hits[0].ID != "mem_cli" || hits[0].TrustLabel != "owner_input" {
		t.Fatalf("hits = %+v", hits)
	}
	mark, found, err := adapter.ProjectionWatermark(ctx, "sess-1")
	if err != nil || !found || mark != 1 {
		t.Fatalf("watermark = %d, %v, %v", mark, found, err)
	}
	if err := adapter.DeleteSessionDocuments(ctx, "sess-1"); err != nil {
		t.Fatalf("DeleteSessionDocuments(): %v", err)
	}
}

func TestBuildMemoryRuntime(t *testing.T) {
	db := newMemoryTestDB(t)
	cfg := config.Default()
	runtime, err := buildMemoryRuntime(context.Background(), &cfg, db, nil, nil)
	if err != nil {
		t.Fatalf("buildMemoryRuntime(): %v", err)
	}
	if runtime.provider == nil {
		t.Fatal("provider missing")
	}
	if runtime.service == nil {
		t.Fatal("service missing")
	}
	if runtime.model != nil || runtime.summarizer != nil {
		t.Errorf("summary model/summarizer = %+v, %+v; want none without a summarize model definition", runtime.model, runtime.summarizer)
	}
	if observer := memoryRecorderObserver(nil); observer != nil {
		t.Error("nil recorder produced an observer")
	}
	recorder, err := telemetry.NewMemoryRecorder(nil)
	if err != nil {
		t.Fatalf("NewMemoryRecorder(): %v", err)
	}
	if observer := memoryRecorderObserver(recorder); observer == nil {
		t.Error("observer missing")
	}
}

type countingSummaryLLM struct {
	answer string
	calls  int
}

func (f *countingSummaryLLM) Name() string { return "counting-summary" }

func (f *countingSummaryLLM) GenerateContent(context.Context, *adkmodel.LLMRequest, bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		f.calls++
		yield(&adkmodel.LLMResponse{Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: f.answer}}}}, nil)
	}
}

type recordingSummary struct {
	text  string
	calls int
	last  *memory.SummaryPrompt
}

func (s *recordingSummary) Summarize(_ context.Context, prompt *memory.SummaryPrompt) (string, error) {
	s.calls++
	s.last = prompt
	return s.text, nil
}

func registerSummaryLLM(t *testing.T, name string, llm adkmodel.LLM) {
	t.Helper()
	adkmodel.Register("^"+regexp.QuoteMeta(name)+"$", func(context.Context, string) (adkmodel.LLM, error) {
		return llm, nil
	})
}

func summaryDefinition(protocol, modelName string) config.ModelDefinition {
	return config.ModelDefinition{
		Protocol: protocol,
		Model:    modelName,
		Capabilities: config.ModelCapabilities{
			Tokenizer:        "cl100k_base",
			ContextTokens:    200000,
			StructuredOutput: true,
		},
	}
}

func seedMemorySession(t *testing.T, db *sql.DB, sessionID string) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(),
		`INSERT INTO session (id, owner_id, metadata_json, created_at, updated_at) VALUES (?,?,?,?,?)`,
		sessionID, "owner-1", `{}`, "2026-09-13T00:00:00Z", "2026-09-13T00:00:00Z"); err != nil {
		t.Fatalf("seed session: %v", err)
	}
}

func seedRawMemoryDocs(t *testing.T, adapter *memoryDocumentStore, sessionID string, count int) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	for i := 1; i <= count; i++ {
		document := &memory.StoredDocument{
			ID:           "mem-" + sessionID + "-" + strconv.Itoa(i),
			OwnerID:      "owner-1",
			SessionID:    sessionID,
			Kind:         memory.KindEventText,
			FromSequence: int64(i),
			ToSequence:   int64(i),
			Content:      "backup note " + strconv.Itoa(i),
			TrustLabel:   "owner_input",
			CreatedAt:    now,
		}
		if _, err := adapter.UpsertDocument(ctx, document); err != nil {
			t.Fatalf("seed raw %d: %v", i, err)
		}
	}
}

func recallTurnRequest(sessionID, text string) *runtime.TurnRequest {
	return &runtime.TurnRequest{
		TurnID:      "turn-1",
		SessionID:   sessionID,
		PrincipalID: "owner-1",
		Parts:       []runtimeingress.InputPart{{Text: text}},
	}
}

func TestBuildMemorySummaryDefaultsToAuxiliaryRoute(t *testing.T) {
	ctx := context.Background()
	suffix := strconv.FormatInt(time.Now().UnixNano(), 10)
	auxModel := "aux-" + suffix
	registerSummaryLLM(t, "auxiliary", &countingSummaryLLM{answer: "digest"})
	cfg := config.Default()
	cfg.Models.Routing = nil
	cfg.Models.Definitions = map[string]config.ModelDefinition{
		"primary": summaryDefinition("primary-protocol", "primary-"+suffix),
		"aux":     summaryDefinition("aux-protocol", auxModel),
	}
	cfg.ModelRoutes = map[string]config.ModelRoute{
		"auxiliary": {Candidates: []string{"aux"}},
	}
	model, summarizer := buildMemorySummary(ctx, &cfg, nil, nil)
	if model == nil || summarizer == nil {
		t.Fatal("summary route not resolved")
	}
	if model.Protocol != "aux-protocol" || model.Name != auxModel {
		t.Errorf("summary model = %+v, want the auxiliary route definition", model)
	}

	db := newMemoryTestDB(t)
	seedMemorySession(t, db, "sess-route")
	adapter := &memoryDocumentStore{store: store.NewMemoryStore(db)}
	seedRawMemoryDocs(t, adapter, "sess-route", 6)
	service, err := memory.NewService(adapter, nil, &memory.Config{RangeThreshold: 6, RangeLiveWindow: 2})
	if err != nil {
		t.Fatalf("NewService(): %v", err)
	}
	recorder := &recordingSummary{text: "digest"}
	if _, summarized, err := service.SummarizeClosedRange(ctx, "owner-1", "sess-route", model, recorder); err != nil {
		t.Fatalf("SummarizeClosedRange(): %v", err)
	} else if !summarized {
		t.Fatal("range not summarized")
	}
	summaries, err := adapter.ListSessionDocumentsByKind(ctx, "owner-1", "sess-route", memory.KindSummary, 10)
	if err != nil || len(summaries) != 1 {
		t.Fatalf("summaries = %d, %v; want 1", len(summaries), err)
	}
	if summaries[0].ModelName != auxModel || summaries[0].ModelProtocol != "aux-protocol" {
		t.Errorf("summary model = %q/%q, want %q/aux-protocol", summaries[0].ModelProtocol, summaries[0].ModelName, auxModel)
	}
}

func TestBuildMemorySummaryHonorsSummarizeOverride(t *testing.T) {
	suffix := strconv.FormatInt(time.Now().UnixNano(), 10)
	route := "summary-route-" + suffix
	overrideModel := "override-" + suffix
	registerSummaryLLM(t, route, &countingSummaryLLM{answer: "digest"})
	cfg := config.Default()
	cfg.Models.Routing = map[string]string{"summarize": route}
	cfg.Models.Definitions = map[string]config.ModelDefinition{
		"primary":  summaryDefinition("primary-protocol", "primary-"+suffix),
		"override": summaryDefinition("override-protocol", overrideModel),
	}
	cfg.ModelRoutes = map[string]config.ModelRoute{
		route: {Candidates: []string{"override"}},
	}
	model, summarizer := buildMemorySummary(context.Background(), &cfg, nil, nil)
	if model == nil || summarizer == nil {
		t.Fatal("summary route not resolved")
	}
	if model.Protocol != "override-protocol" || model.Name != overrideModel {
		t.Errorf("summary model = %+v, want the summarize override definition", model)
	}
}

func TestMemoryRuntimeRecallStaysRaw(t *testing.T) {
	ctx := context.Background()
	suffix := strconv.FormatInt(time.Now().UnixNano(), 10)
	route := "memory-recall-" + suffix
	llm := &countingSummaryLLM{answer: "digest"}
	registerSummaryLLM(t, route, llm)
	cfg := config.Default()
	cfg.Memory.RangeThreshold = 6
	cfg.Memory.RangeLiveWindow = 2
	cfg.Models.Routing = map[string]string{"summarize": route}
	cfg.Models.Definitions = map[string]config.ModelDefinition{
		"aux": summaryDefinition("aux-protocol", "aux-"+suffix),
	}
	cfg.ModelRoutes = map[string]config.ModelRoute{
		route: {Candidates: []string{"aux"}},
	}
	db := newMemoryTestDB(t)
	runtime, err := buildMemoryRuntime(ctx, &cfg, db, nil, nil)
	if err != nil {
		t.Fatalf("buildMemoryRuntime(): %v", err)
	}
	if runtime.model == nil || runtime.summarizer == nil {
		t.Fatal("summary route not built for the background pass")
	}
	adapter := &memoryDocumentStore{store: store.NewMemoryStore(db)}
	seedMemorySession(t, db, "sess-below")
	seedRawMemoryDocs(t, adapter, "sess-below", 2)

	evidence, err := runtime.provider.RecallTurn(ctx, recallTurnRequest("sess-below", "backup"))
	if err != nil {
		t.Fatalf("RecallTurn(): %v", err)
	}
	if evidence == nil || len(evidence.Documents) != 2 {
		t.Fatalf("evidence = %+v, want raw documents", evidence)
	}
	if llm.calls != 0 {
		t.Errorf("summarizer calls = %d, want 0 below the range threshold", llm.calls)
	}
	summaries, err := adapter.ListSessionDocumentsByKind(ctx, "owner-1", "sess-below", memory.KindSummary, 10)
	if err != nil || len(summaries) != 0 {
		t.Errorf("summaries = %d, %v; want none below the range threshold", len(summaries), err)
	}

	seedMemorySession(t, db, "sess-closed")
	seedRawMemoryDocs(t, adapter, "sess-closed", 6)
	recorder := &recordingSummary{text: "backup digest"}
	backgroundModel := &memory.SummaryModel{
		Protocol:         "aux-protocol",
		Name:             "aux-" + suffix,
		Tokenizer:        "cl100k_base",
		ContextTokens:    200000,
		StructuredOutput: true,
	}
	if _, summarized, err := runtime.service.SummarizeClosedRange(ctx, "owner-1", "sess-closed", backgroundModel, recorder); err != nil {
		t.Fatalf("SummarizeClosedRange(): %v", err)
	} else if !summarized {
		t.Fatal("closed range not summarized")
	}
	before := llm.calls
	evidence, err = runtime.provider.RecallTurn(ctx, recallTurnRequest("sess-closed", "backup"))
	if err != nil {
		t.Fatalf("RecallTurn(): %v", err)
	}
	if llm.calls != before {
		t.Errorf("recall invoked the summarizer %d times, want zero", llm.calls-before)
	}
	if recorder.calls != 1 {
		t.Fatalf("background summarizer calls = %d, want 1", recorder.calls)
	}
	for _, document := range recorder.last.Documents {
		if document.PromptVersion != "" {
			t.Errorf("background summary input includes derived document %+v, want raw only", document)
		}
	}
	derived := false
	for _, document := range evidence.Documents {
		if document.Trust == approval.TrustDerivedUntrusted {
			derived = true
		}
	}
	if !derived {
		t.Errorf("recall evidence = %+v, want the existing summary document", evidence)
	}
}
