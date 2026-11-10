package cli

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/modules/knowledge"
	"github.com/anggasct/aura/internal/platform/health"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/turn"
	runtimeingress "github.com/anggasct/aura/internal/platform/turn/ingress"
)

const (
	returnsAndShippingDocument = "# Returns\nCustomers get their money back within thirty days.\n\n# Shipping\nOrders are delivered in five days.\n"
	meaningQuery               = "What is your reimbursement policy?"
	moneyBackText              = "money back within thirty days"
	vectorWait                 = 30 * time.Second
)

var faqVectorBytes = knowledge.VectorBytes(1, 4)

func meaningVector(input string, width int) []float64 {
	lower := strings.ToLower(input)
	vector := make([]float64, width)
	switch {
	case strings.Contains(lower, "money back"), strings.Contains(lower, "reimburse"):
		vector[0] = 1
	case strings.Contains(lower, "deliver"), strings.Contains(lower, "ship"):
		vector[1] = 1
	default:
		vector[2] = 1
	}
	return vector
}

func newVectorTurnRig(t *testing.T, extraEmbedding string) *knowledgeTurnRig {
	t.Helper()
	provider := newEmbeddingProvider(t, 4)
	provider.vectorFor = meaningVector
	return newKnowledgeTurnRigWith(t, embeddingYAML(provider.server.URL+"/v1", 4, "    batch_size: 1\n"+extraEmbedding), provider)
}

func (r *knowledgeTurnRig) embedAll(t *testing.T, collection string) {
	t.Helper()
	result, err := r.service.Embed(t.Context(), collection)
	if err != nil || !result.Completed {
		t.Fatalf("Embed(%s) = %+v, %v", collection, result, err)
	}
}

func (r *knowledgeTurnRig) settle(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), vectorWait)
	defer cancel()
	if err := r.vectors.index.Idle(ctx); err != nil {
		t.Fatalf("the vector index never went idle: %v", err)
	}
}

func (r *knowledgeTurnRig) searchOnce(t *testing.T, scope []string, query string, class principal.Class) knowledge.Retrieval {
	t.Helper()
	found, err := r.retriever.Search(t.Context(), &knowledge.SearchRequest{Scope: scope, Query: query, Depth: knowledge.SearchDepth, Class: class})
	if err != nil {
		t.Fatalf("Search(%q): %v", query, err)
	}
	return found
}

func (r *knowledgeTurnRig) warm(t *testing.T, scope ...string) {
	t.Helper()
	r.searchOnce(t, scope, meaningQuery, principal.ClassOwner)
	r.settle(t)
}

func (r *knowledgeTurnRig) queriesSent() int { return r.provider.inputCount() }

func (r *knowledgeTurnRig) lastMode(t *testing.T) string {
	t.Helper()
	payloads := r.retrieved(t)
	if len(payloads) == 0 {
		t.Fatal("no kb.retrieved.v1 event")
	}
	var payload struct {
		Mode    string `json:"mode"`
		Outcome string `json:"outcome"`
	}
	if err := json.Unmarshal([]byte(payloads[len(payloads)-1]), &payload); err != nil {
		t.Fatalf("decode event: %v", err)
	}
	return payload.Mode
}

func (r *knowledgeTurnRig) toolSearch(t *testing.T, scope []string, query string, class principal.Class) (mode string, results int) {
	t.Helper()
	searcher := &knowledgeToolSearcher{retriever: r.retriever}
	raw, err := searcher.SearchKnowledge(t.Context(), scope, query, 5, class)
	if err != nil {
		t.Fatalf("SearchKnowledge: %v", err)
	}
	var output struct {
		Mode    string            `json:"mode"`
		Results []json.RawMessage `json:"results"`
	}
	if err := json.Unmarshal(raw, &output); err != nil {
		t.Fatalf("decode output %s: %v", raw, err)
	}
	return output.Mode, len(output.Results)
}

func newEmbeddedRig(t *testing.T, extraEmbedding string) *knowledgeTurnRig {
	t.Helper()
	rig := newVectorTurnRig(t, extraEmbedding)
	rig.add(t, "support", "policy.md", returnsAndShippingDocument)
	rig.add(t, "faq", "faq.md", "# Money\nYou can get your money back within thirty days of purchase.\n")
	rig.embedAll(t, "support")
	rig.embedAll(t, "faq")
	rig.warm(t, "support")
	rig.warm(t, "faq")
	rig.provider.reset()
	return rig
}

func TestKnowledgeTurnFindsAChunkByMeaningOnceCoverageIsComplete(t *testing.T) {
	rig := newVectorTurnRig(t, "")
	rig.add(t, "support", "policy.md", returnsAndShippingDocument)

	rig.provider.reset()
	rig.turn(t, "lobby", principal.ClassOwner, meaningQuery)
	if strings.Contains(rig.model.last(t).joined(), moneyBackText) || rig.lastMode(t) != knowledge.ModeLexical || rig.queriesSent() != 0 {
		t.Fatalf("before opt-in: block present, mode %q, sends %d", rig.lastMode(t), rig.queriesSent())
	}

	rig.provider.mu.Lock()
	rig.provider.failCall = 2
	rig.provider.mu.Unlock()
	if _, err := rig.service.Embed(t.Context(), "support"); err == nil {
		t.Fatal("the failing provider call did not stop the run")
	}
	rig.provider.mu.Lock()
	rig.provider.failCall = 0
	rig.provider.mu.Unlock()
	sent := rig.provider.callCount()
	rig.warm(t, "support")
	rig.turn(t, "lobby", principal.ClassOwner, meaningQuery)
	if strings.Contains(rig.model.last(t).joined(), moneyBackText) || rig.lastMode(t) != knowledge.ModeLexical || rig.provider.callCount() != sent {
		t.Fatalf("partial collection: mode %q, provider calls %d -> %d", rig.lastMode(t), sent, rig.provider.callCount())
	}

	rig.embedAll(t, "support")
	rig.turn(t, "lobby", principal.ClassOwner, meaningQuery)
	if rig.lastMode(t) != knowledge.ModeLexical {
		t.Fatalf("the first retrieval after completion answered %q, want lexical while the collection loads", rig.lastMode(t))
	}
	rig.settle(t)
	rig.provider.reset()
	rig.turn(t, "lobby", principal.ClassOwner, meaningQuery)
	if !strings.Contains(rig.model.last(t).joined(), moneyBackText) || rig.lastMode(t) != knowledge.ModeHybrid {
		t.Fatalf("complete collection: mode %q, block:\n%s", rig.lastMode(t), rig.model.last(t).joined())
	}
	if rig.queriesSent() != 1 || rig.provider.calls[0].inputs[0] != meaningQuery {
		t.Errorf("provider calls = %+v, want the one query text", rig.provider.calls)
	}
}

func TestKnowledgeSearchToolReturnsTheModeAndFindsAChunkByMeaning(t *testing.T) {
	rig := newEmbeddedRig(t, "")
	mode, results := rig.toolSearch(t, []string{"support"}, meaningQuery, principal.ClassOwner)
	if mode != knowledge.ModeHybrid || results == 0 {
		t.Fatalf("kb_search = mode %q, %d results", mode, results)
	}
}

func TestKnowledgeQueriesLeaveTheHostOnlyWhenEveryGateHolds(t *testing.T) {
	rig := newEmbeddedRig(t, "")
	provider, err := knowledge.NewContextProvider(rig.retriever)
	if err != nil {
		t.Fatalf("NewContextProvider: %v", err)
	}

	tests := map[string]struct {
		class     principal.Class
		wantSends int
		wantMode  string
	}{
		"owner":   {class: principal.ClassOwner, wantSends: 1, wantMode: knowledge.ModeHybrid},
		"trusted": {class: principal.ClassTrusted, wantSends: 1, wantMode: knowledge.ModeHybrid},
		"public":  {class: principal.ClassPublic, wantSends: 0, wantMode: knowledge.ModeLexical},
		"empty":   {class: "", wantSends: 0, wantMode: knowledge.ModeLexical},
	}
	for name, test := range tests {
		t.Run("turn context "+name, func(t *testing.T) {
			rig.provider.reset()
			evidence, err := provider.KnowledgeContext(t.Context(), &turn.TurnRequest{
				Knowledge: []string{"support", "faq"}, PrincipalClass: test.class,
				Parts: []runtimeingress.InputPart{{Text: meaningQuery}},
			})
			if err != nil || evidence.Mode != test.wantMode || rig.queriesSent() != test.wantSends {
				t.Fatalf("evidence mode %q, sends %d, error %v; want %q and %d", evidence.Mode, rig.queriesSent(), err, test.wantMode, test.wantSends)
			}
		})
		t.Run("kb_search "+name, func(t *testing.T) {
			rig.provider.reset()
			mode, _ := rig.toolSearch(t, []string{"support", "faq"}, meaningQuery, test.class)
			if mode != test.wantMode || rig.queriesSent() != test.wantSends {
				t.Fatalf("mode %q, sends %d; want %q and %d", mode, rig.queriesSent(), test.wantMode, test.wantSends)
			}
		})
	}

	for _, class := range []principal.Class{principal.ClassOwner, principal.ClassTrusted, principal.ClassPublic} {
		t.Run("engine turn "+string(class), func(t *testing.T) {
			rig.provider.reset()
			rig.turn(t, "lobby", class, meaningQuery)
			wantSends, wantMode := 1, knowledge.ModeHybrid
			if class == principal.ClassPublic {
				wantSends, wantMode = 0, knowledge.ModeLexical
			}
			if rig.queriesSent() != wantSends || rig.lastMode(t) != wantMode {
				t.Fatalf("sends %d, mode %q; want %d and %q", rig.queriesSent(), rig.lastMode(t), wantSends, wantMode)
			}
		})
	}
}

func TestKnowledgePublicQueriesAreSentWhenTheOwnerAllowsThem(t *testing.T) {
	rig := newEmbeddedRig(t, "    public_queries: true\n")
	rig.turn(t, "lobby", principal.ClassPublic, meaningQuery)
	if rig.queriesSent() != 1 || rig.lastMode(t) != knowledge.ModeHybrid {
		t.Fatalf("sends %d, mode %q, want one send in hybrid mode", rig.queriesSent(), rig.lastMode(t))
	}
	if mode, _ := rig.toolSearch(t, []string{"faq"}, meaningQuery, principal.ClassPublic); mode != knowledge.ModeHybrid {
		t.Errorf("kb_search mode = %q", mode)
	}
}

func TestKnowledgeQueryMatchingAVaultValueIsNeverSent(t *testing.T) {
	rig := newEmbeddedRig(t, "")
	secretQuery := meaningQuery + " kb-turn-test-model-key-value"
	found := rig.searchOnce(t, []string{"support"}, secretQuery, principal.ClassOwner)
	if rig.queriesSent() != 0 || found.Mode != knowledge.ModeLexical {
		t.Fatalf("a query holding a vault value: sends %d, mode %q", rig.queriesSent(), found.Mode)
	}
	if mode, _ := rig.toolSearch(t, []string{"support"}, secretQuery, principal.ClassOwner); mode != knowledge.ModeLexical || rig.queriesSent() != 0 {
		t.Fatalf("kb_search with a vault value: mode %q, sends %d", mode, rig.queriesSent())
	}
	rig.searchOnce(t, []string{"support"}, meaningQuery, principal.ClassOwner)
	if rig.queriesSent() != 1 {
		t.Errorf("the clean control query sent %d, want 1", rig.queriesSent())
	}
}

func TestKnowledgeColdAndPartialScopesSendNothing(t *testing.T) {
	rig := newVectorTurnRig(t, "")
	rig.add(t, "support", "policy.md", returnsAndShippingDocument)
	rig.add(t, "faq", "faq.md", "# Money\nYou can get your money back.\n")
	rig.embedAll(t, "support")
	rig.provider.reset()

	if found := rig.searchOnce(t, []string{"support"}, meaningQuery, principal.ClassOwner); found.Mode != knowledge.ModeLexical || rig.queriesSent() != 0 {
		t.Fatalf("a collection that is not loaded yet: mode %q, sends %d", found.Mode, rig.queriesSent())
	}
	rig.settle(t)
	if found := rig.searchOnce(t, []string{"faq"}, meaningQuery, principal.ClassOwner); found.Mode != knowledge.ModeLexical || rig.queriesSent() != 0 {
		t.Fatalf("a collection with no vectors: mode %q, sends %d", found.Mode, rig.queriesSent())
	}
	rig.settle(t)
	if found := rig.searchOnce(t, []string{"support"}, meaningQuery, principal.ClassOwner); found.Mode != knowledge.ModeHybrid || rig.queriesSent() != 1 {
		t.Fatalf("once loaded: mode %q, sends %d", found.Mode, rig.queriesSent())
	}
	rig.provider.reset()
	if found := rig.searchOnce(t, []string{"faq"}, meaningQuery, principal.ClassOwner); found.Mode != knowledge.ModeLexical || rig.queriesSent() != 0 {
		t.Fatalf("a partial scope of only unqualified collections: mode %q, sends %d", found.Mode, rig.queriesSent())
	}
}

func TestKnowledgeServerStopsSendingWhenTheCLIReembedsUnderAnotherFingerprint(t *testing.T) {
	rig := newEmbeddedRig(t, "")
	if rig.vectors.index.Used() == 0 {
		t.Fatal("nothing was loaded")
	}

	changed := *rig.cfg
	knowledgeCopy := *rig.cfg.Knowledge
	embeddingCopy := *rig.cfg.Knowledge.Embedding
	embeddingCopy.Model = "another-model"
	knowledgeCopy.Embedding = &embeddingCopy
	changed.Knowledge = &knowledgeCopy
	cli, err := newKnowledgeService(&changed, rig.db, nil)
	if err != nil {
		t.Fatalf("newKnowledgeService: %v", err)
	}
	if result, err := cli.Embed(t.Context(), "support"); err != nil || !result.Completed {
		t.Fatalf("Embed under the new model = %+v, %v", result, err)
	}
	rig.provider.reset()

	found := rig.searchOnce(t, []string{"support"}, meaningQuery, principal.ClassOwner)
	if found.Mode != knowledge.ModeLexical || rig.queriesSent() != 0 || rig.vectors.index.Used() != faqVectorBytes {
		t.Fatalf("after the fingerprint changed: mode %q, sends %d, memory %d", found.Mode, rig.queriesSent(), rig.vectors.index.Used())
	}
	rig.settle(t)
	if found := rig.searchOnce(t, []string{"support"}, meaningQuery, principal.ClassOwner); found.Mode != knowledge.ModeLexical || rig.queriesSent() != 0 {
		t.Fatalf("a collection under another fingerprint was loaded: mode %q, sends %d", found.Mode, rig.queriesSent())
	}
}

func TestKnowledgeServerSeesACLIChangeAtItsNextRetrieval(t *testing.T) {
	rig := newEmbeddedRig(t, "")

	rig.add(t, "support", "warranty.md", "# Warranty\nWe will reimburse repairs under warranty.\n")
	rig.embedAll(t, "support")
	rig.provider.reset()
	if found := rig.searchOnce(t, []string{"support"}, meaningQuery, principal.ClassOwner); found.Mode != knowledge.ModeLexical || rig.queriesSent() != 0 {
		t.Fatalf("the retrieval after a CLI change: mode %q, sends %d, want a lexical answer while it reloads", found.Mode, rig.queriesSent())
	}
	rig.settle(t)
	found := rig.searchOnce(t, []string{"support"}, meaningQuery, principal.ClassOwner)
	var warranty bool
	for i := range found.Hits {
		warranty = warranty || strings.Contains(found.Hits[i].Content, "reimburse repairs")
	}
	if found.Mode != knowledge.ModeHybrid || !warranty {
		t.Fatalf("after the reload: mode %q, hits %+v, want the new chunk by meaning", found.Mode, found.Hits)
	}

	outcome, err := rig.service.EmbedOff(t.Context(), "support", true)
	if err != nil || !outcome.Applied {
		t.Fatalf("EmbedOff = %+v, %v", outcome, err)
	}
	rig.provider.reset()
	erased := rig.searchOnce(t, []string{"support"}, meaningQuery, principal.ClassOwner)
	if erased.Mode != knowledge.ModeLexical || rig.queriesSent() != 0 || rig.vectors.index.Used() != faqVectorBytes {
		t.Fatalf("after --off: mode %q, sends %d, memory %d", erased.Mode, rig.queriesSent(), rig.vectors.index.Used())
	}
}

func TestKnowledgeOverLimitCollectionStaysLexicalAndSendsNothing(t *testing.T) {
	rig := newVectorTurnRig(t, "")
	rig.add(t, "support", "policy.md", returnsAndShippingDocument)
	rig.embedAll(t, "support")
	rig.cfg.Knowledge.Embedding.MaxMemory = 100
	pool, err := openKnowledgeReadPool(t.Context(), rig.dbPath)
	if err != nil {
		t.Fatalf("openKnowledgeReadPool: %v", err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	vectors, enabled, err := startKnowledgeVectors(t.Context(), rig.cfg, pool, rig.dbPath)
	if err != nil || !enabled {
		t.Fatalf("startKnowledgeVectors = %v, %v", enabled, err)
	}
	t.Cleanup(vectors.close)
	retriever, err := newKnowledgeRetriever(rig.cfg, pool, vectors.options()...)
	if err != nil {
		t.Fatalf("newKnowledgeRetriever: %v", err)
	}
	rig.provider.reset()
	for range 2 {
		found, err := retriever.Search(t.Context(), &knowledge.SearchRequest{Scope: []string{"support"}, Query: meaningQuery, Depth: knowledge.SearchDepth, Class: principal.ClassOwner})
		if err != nil || found.Mode != knowledge.ModeLexical {
			t.Fatalf("Search = %+v, %v, want a lexical answer", found, err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), vectorWait)
		if err := vectors.index.Idle(ctx); err != nil {
			t.Fatalf("Idle: %v", err)
		}
		cancel()
	}
	if rig.queriesSent() != 0 || vectors.index.Used() != 0 {
		t.Errorf("sends %d, memory %d, want nothing sent and nothing held", rig.queriesSent(), vectors.index.Used())
	}

	findings := embeddingFindings(t, rig.kbRetrievalRig)
	var limited []health.Finding
	for i := range findings {
		if findings[i].Code == "kb_memory_limit" {
			limited = append(limited, findings[i])
		}
	}
	if len(limited) == 0 || limited[0].Status != health.StatusDegraded || !strings.Contains(limited[0].Detail, "max_memory") {
		t.Fatalf("status did not report the memory limit: %+v", findings)
	}
}

func TestKnowledgeUnresolvedEmbeddingCredentialKeepsLexicalRetrievalWorking(t *testing.T) {
	rig := newVectorTurnRig(t, "    credential_ref: env://KB_VECTOR_UNSET_KEY\n")
	rig.add(t, "support", "policy.md", "# Refunds\nRefunds are issued within thirty days.\n")
	if rig.vectors != nil {
		t.Fatal("vector retrieval started without a resolvable credential")
	}
	rig.provider.reset()
	rig.turn(t, "lobby", principal.ClassOwner, "How do refunds work?")
	if !strings.Contains(rig.model.last(t).joined(), "Refunds are issued") || rig.lastMode(t) != knowledge.ModeLexical || rig.queriesSent() != 0 {
		t.Fatalf("lexical retrieval broke: mode %q, sends %d", rig.lastMode(t), rig.queriesSent())
	}
}

func TestKnowledgeSlowEmbeddingProviderFallsBackToLexicalWithinTheBudget(t *testing.T) {
	rig := newEmbeddedRig(t, "")
	rig.add(t, "support", "refunds.md", "# Reimbursement\nReimbursement requests go through the portal.\n")
	rig.embedAll(t, "support")
	rig.warm(t, "support")
	release := make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	rig.provider.mu.Lock()
	rig.provider.onRequest = func(int) { <-release }
	rig.provider.mu.Unlock()

	started := time.Now()
	mode, results := rig.toolSearch(t, []string{"support"}, meaningQuery, principal.ClassOwner)
	if mode != knowledge.ModeLexicalFallback || results == 0 {
		t.Fatalf("kb_search = mode %q with %d results, want lexical hits after the embedding limit", mode, results)
	}
	if took := time.Since(started); took >= knowledge.RetrievalBudget {
		t.Errorf("kb_search took %s, inside the %s budget it should leave lexical time", took, knowledge.RetrievalBudget)
	}

	budgeted, cancel := context.WithTimeout(t.Context(), knowledge.RetrievalBudget)
	defer cancel()
	provider, err := knowledge.NewContextProvider(rig.retriever)
	if err != nil {
		t.Fatalf("NewContextProvider: %v", err)
	}
	evidence, err := provider.KnowledgeContext(budgeted, &turn.TurnRequest{
		Knowledge: []string{"support"}, PrincipalClass: principal.ClassOwner,
		Parts: []runtimeingress.InputPart{{Text: meaningQuery}},
	})
	if err != nil || evidence.Mode != knowledge.ModeLexicalFallback || evidence.Retrieved == 0 {
		t.Fatalf("turn context = %+v, %v, want lexical hits in fallback mode", evidence, err)
	}
	once.Do(func() { close(release) })
}

func TestBuildKnowledgeRuntimeStopsItsLoaderWithTheRuntime(t *testing.T) {
	rig := newVectorTurnRig(t, "")
	registry := personaRegistry(t, rig.cfgPath)
	runtimeState, enabled, err := buildKnowledgeRuntime(t.Context(), rig.cfg, registry)
	if err != nil || !enabled || runtimeState.vectors == nil {
		t.Fatalf("buildKnowledgeRuntime = %v, %v, vectors %v", enabled, err, runtimeState.vectors)
	}
	done := runtimeState.vectors.done
	runtimeState.close()
	select {
	case <-done:
	default:
		t.Fatal("the loader goroutine was still running after close")
	}
}
