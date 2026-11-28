//go:build durable

package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/modules/knowledge"
	"github.com/anggasct/aura/internal/platform/durable"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/operation/operationtest"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/sqlite"
	"github.com/anggasct/aura/internal/platform/tools"
	"github.com/anggasct/aura/internal/platform/turn"
	runtimeadk "github.com/anggasct/aura/internal/platform/turn/adk"
	runtimeengine "github.com/anggasct/aura/internal/platform/turn/engine"
)

const (
	approvalCalls       = "approval"
	skillRenders        = "render"
	skillContextCalls   = "skill-context"
	knowledgeCalls      = "knowledge"
	embeddingCalls      = "embed"
	lexicalSearches     = "lexical"
	queuedMarker        = "queued"
	originalSkillBody   = "ORIGINAL-SKILL-BODY"
	pinnedInvocation    = "inv-pinned"
	originalKnowledge   = "ORIGINAL-KNOWLEDGE"
	changedKnowledge    = "CHANGED-KNOWLEDGE"
	originalChunk       = "kbc_original"
	semanticKnowledge   = "SEMANTIC-KNOWLEDGE"
	lexicalKnowledge    = "LEXICAL-KNOWLEDGE"
	knowledgeScope      = "support"
	knowledgeRetrievals = 3
)

type stubTool struct {
	name           string
	approval       bool
	fails          bool
	gatedInAdapter bool
	effectful      bool
}

type lifeWiring struct {
	renderer     runtimeengine.SkillRenderer
	skillContext turn.ContextProvider
	knowledge    turn.ContextProvider
	compactor    runtimeadk.HistoryCompactor
	capabilities runtimeadk.CapabilityLookup
}

type ledgerOperation struct {
	id         string
	gateBefore bool
	gateAfter  bool
}

type turnScenario struct {
	steps         []string
	arguments     map[string]map[string]any
	operations    []ledgerOperation
	discovery     bool
	tools         []stubTool
	gated         string
	approvingLife int
	requests      []turn.TurnRequest
	seed          func(t *testing.T, db *sql.DB)
	wiring        func(t *testing.T, db *sql.DB, ledger *turnLedger) lifeWiring
	firstLife     func(t *testing.T, life *turnLife)
}

var turnScenarios = map[string]turnScenario{
	"tools": {
		steps:    []string{"tool-a", "tool-b", textStep},
		tools:    []stubTool{{name: "tool-a"}, {name: "tool-b"}},
		gated:    "tool-b",
		requests: []turn.TurnRequest{suiteRequest("turn-1", "USER-TEXT-TOOLS")},
	},
	"approval": {
		steps:         []string{"tool-c", textStep},
		tools:         []stubTool{{name: "tool-c", approval: true, gatedInAdapter: true}},
		approvingLife: 1,
		requests:      []turn.TurnRequest{suiteRequest("turn-1", "USER-TEXT-APPROVAL")},
	},
	"skill": {
		steps:    []string{"tool-a", textStep},
		tools:    []stubTool{{name: "tool-a"}},
		gated:    "tool-a",
		requests: []turn.TurnRequest{pinnedRequest("turn-1", "USER-TEXT-SKILL")},
		wiring:   skillAcrossLives,
	},
	"knowledge": {
		steps:    []string{"tool-a", textStep},
		tools:    []stubTool{{name: "tool-a"}},
		gated:    "tool-a",
		requests: []turn.TurnRequest{knowledgeRequest("turn-1", "USER-TEXT-KNOWLEDGE")},
		wiring:   knowledgeAcrossLives,
	},
	"embedding": {
		steps:    []string{"tool-a", textStep},
		tools:    []stubTool{{name: "tool-a"}},
		gated:    "tool-a",
		requests: []turn.TurnRequest{knowledgeRequest("turn-1", "how do I get my money back")},
		wiring:   embeddingAcrossLives,
	},
	"queued": {
		steps:     []string{"tool-a", textStep, textStep},
		tools:     []stubTool{{name: "tool-a"}},
		gated:     "tool-a",
		requests:  []turn.TurnRequest{suiteRequest("turn-1", "USER-TEXT-QUEUED-FIRST"), suiteRequest("turn-2", "USER-TEXT-QUEUED-SECOND")},
		firstLife: queueBehindGatedTurn,
	},
	"failed-tool": {
		steps:    []string{"tool-a", "tool-b", textStep},
		tools:    []stubTool{{name: "tool-a", fails: true}, {name: "tool-b"}},
		gated:    "tool-b",
		requests: []turn.TurnRequest{suiteRequest("turn-1", "USER-TEXT-FAILED-TOOL")},
	},
	"cancel": {
		steps:    []string{"tool-a", textStep},
		tools:    []stubTool{{name: "tool-a"}},
		gated:    "tool-a",
		requests: []turn.TurnRequest{suiteRequest("turn-running", "USER-TEXT-CANCEL-RUNNING"), suiteRequest("turn-queued", "USER-TEXT-CANCEL-QUEUED")},
	},
	"transient-skill": {
		steps:    []string{textStep},
		tools:    []stubTool{{name: "tool-a"}},
		requests: []turn.TurnRequest{pinnedRequest("turn-1", "USER-TEXT-TRANSIENT"), suiteRequest("turn-2", "USER-TEXT-AFTER-TRANSIENT")},
		wiring:   transientSkillRender,
	},
	"knowledge-failure": {
		steps:    []string{textStep},
		tools:    []stubTool{{name: "tool-a"}},
		requests: []turn.TurnRequest{knowledgeRequest("turn-1", "USER-TEXT-KNOWLEDGE-FAILURE")},
		wiring:   failingKnowledge,
	},
	"effect-returned": {
		steps:    []string{"tool-e", textStep},
		tools:    []stubTool{{name: "tool-e", effectful: true}},
		requests: []turn.TurnRequest{suiteRequest("turn-1", "USER-TEXT-EFFECT-RETURNED")},
	},
	"operation-replay": {
		steps:         []string{operationtest.ToolOf(operationtest.Change), textStep},
		arguments:     map[string]map[string]any{operationtest.ToolOf(operationtest.Change): {"subject": "replayed"}},
		operations:    []ledgerOperation{{id: operationtest.Change, gateBefore: true}},
		requests:      []turn.TurnRequest{suiteRequest("turn-1", "USER-TEXT-OPERATION-REPLAY")},
		approvingLife: 1,
	},
	"operation-committed": {
		steps:         []string{operationtest.ToolOf(operationtest.Change), textStep},
		arguments:     map[string]map[string]any{operationtest.ToolOf(operationtest.Change): {"subject": "committed"}},
		operations:    []ledgerOperation{{id: operationtest.Change, gateAfter: true}},
		requests:      []turn.TurnRequest{suiteRequest("turn-1", "USER-TEXT-OPERATION-COMMITTED")},
		approvingLife: 1,
	},
	"operation-destructive": {
		steps:         []string{operationtest.ToolOf(operationtest.Delete), textStep},
		arguments:     map[string]map[string]any{operationtest.ToolOf(operationtest.Delete): {"subject": "doomed"}},
		operations:    []ledgerOperation{{id: operationtest.Delete, gateBefore: true}},
		requests:      []turn.TurnRequest{suiteRequest("turn-1", "USER-TEXT-OPERATION-DESTRUCTIVE")},
		approvingLife: 1,
	},
	"operation-search-replay": {
		steps: []string{tools.OperationSearchToolName, operationtest.ToolOf(operationtest.Change), textStep},
		arguments: map[string]map[string]any{
			tools.OperationSearchToolName:              {"group": operationtest.Group},
			operationtest.ToolOf(operationtest.Change): {"subject": "searched"},
		},
		operations:    []ledgerOperation{{id: operationtest.Change, gateBefore: true}},
		discovery:     true,
		requests:      []turn.TurnRequest{suiteRequest("turn-1", "USER-TEXT-OPERATION-SEARCH-REPLAY")},
		approvingLife: 1,
	},
	"lost-lease": {
		steps:    []string{"tool-a", textStep},
		tools:    []stubTool{{name: "tool-a"}},
		gated:    "tool-a",
		requests: []turn.TurnRequest{suiteRequest("turn-1", "USER-TEXT-LOST-LEASE")},
	},
	"compaction-replay": {
		steps:      []string{"tool-a", textStep},
		tools:      []stubTool{{name: "tool-a"}},
		operations: []ledgerOperation{{id: operationtest.Change}},
		discovery:  true,
		gated:      "tool-a",
		requests:   []turn.TurnRequest{suiteRequest("turn-1", "USER-TEXT-COMPACTION-REPLAY")},
		seed:       seedCompactionDurableSession,
		wiring:     compactionAcrossLives,
	},
}

func pinnedRequest(turnID, text string) turn.TurnRequest {
	req := suiteRequest(turnID, text)
	req.Skill = &turn.SkillPin{SkillID: "local/pdf@0123456789ab", Digest: "0123456789abcdef", InvocationID: pinnedInvocation, PolicyVersion: "1"}
	return req
}

func knowledgeRequest(turnID, text string) turn.TurnRequest {
	req := suiteRequest(turnID, text)
	req.Knowledge = []string{knowledgeScope}
	return req
}

func queueBehindGatedTurn(t *testing.T, life *turnLife) {
	t.Helper()
	first, second := &life.scenario.requests[0], &life.scenario.requests[1]
	life.submit(t, first)
	waitUntil(t, "tool-a is entered", life.ledger.exists(t, "tool-a.entered"))
	life.submit(t, second)
	life.waitActivity(t, first.TurnID, 1)
	life.ledger.mark(t, queuedMarker)
}

type ledgerSkill struct {
	ledger *turnLedger
	body   string
	block  string
	err    error
}

func (s *ledgerSkill) RenderSkill(context.Context, string, *turn.SkillPin) (caveat, body string, err error) {
	if _, err := s.ledger.record(skillRenders, ""); err != nil {
		return "", "", err
	}
	return "caveat", s.body, s.err
}

func (s *ledgerSkill) Required(_ *turn.TurnRequest) bool { return false }

func (s *ledgerSkill) TurnContext(ctx context.Context, req *turn.TurnRequest) ([]turn.ContextBlock, error) {
	if req == nil || req.PrincipalClass != principal.ClassOwner {
		return nil, nil
	}
	render := func(context.Context) ([]byte, error) {
		if _, err := s.ledger.record(skillContextCalls, ""); err != nil {
			return nil, err
		}
		return []byte(strings.TrimSpace(s.block)), nil
	}
	var raw []byte
	if scope, ok := durable.TurnScopeFrom(ctx); ok {
		var err error
		raw, err = scope.Invocation().RunAction(ctx, "skill-context", render)
		if err != nil {
			return nil, nil
		}
	} else {
		raw, _ = render(ctx)
	}
	block := strings.TrimSpace(string(raw))
	if block == "" {
		return nil, nil
	}
	return []turn.ContextBlock{{Order: turn.ContextOrderSkills, Content: block}}, nil
}

func skillAcrossLives(_ *testing.T, _ *sql.DB, ledger *turnLedger) lifeWiring {
	stub := &ledgerSkill{ledger: ledger, body: originalSkillBody, block: turn.SkillContextStart + "]\nORIGINAL-BLOCK\n" + turn.SkillContextEnd}
	if ledger.life > 1 {
		stub = &ledgerSkill{ledger: ledger, body: "CHANGED-BODY", block: "CHANGED-BLOCK", err: fault.New(turn.ErrorCodeSkillUnavailable, "skill is quarantined")}
	}
	return lifeWiring{renderer: stub, skillContext: stub}
}

func transientSkillRender(_ *testing.T, _ *sql.DB, ledger *turnLedger) lifeWiring {
	stub := &ledgerSkill{ledger: ledger, block: turn.SkillContextStart + "]\nUNUSED-BLOCK\n" + turn.SkillContextEnd, err: fault.New(sqlite.ErrorCodeUnavailable, "read skill")}
	return lifeWiring{renderer: stub, skillContext: stub}
}

type ledgerKnowledgeIndex struct {
	ledger *turnLedger
	hits   []knowledge.Hit
	err    error
}

func (k *ledgerKnowledgeIndex) ScopeChunks(context.Context, []string) (int, error) {
	return max(len(k.hits), 1), nil
}

func (k *ledgerKnowledgeIndex) TermFrequency(context.Context, []string, string, int) (int, error) {
	return 1, nil
}

func (k *ledgerKnowledgeIndex) Search(_ context.Context, scope, _ []string, _ int) ([]knowledge.Hit, error) {
	encoded, err := json.Marshal(scope)
	if err != nil {
		return nil, err
	}
	if _, err := k.ledger.record(knowledgeCalls, string(encoded)); err != nil {
		return nil, err
	}
	if k.err != nil {
		return nil, k.err
	}
	return k.hits, nil
}

func newLedgerKnowledgeProvider(t *testing.T, idx knowledge.Index) turn.ContextProvider {
	t.Helper()
	retriever, err := knowledge.NewRetriever(idx, emptyVaultSource{})
	if err != nil {
		t.Fatalf("NewRetriever: %v", err)
	}
	provider, err := knowledge.NewContextProvider(retriever, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("NewContextProvider: %v", err)
	}
	return provider
}

func knowledgeAcrossLives(t *testing.T, _ *sql.DB, ledger *turnLedger) lifeWiring {
	t.Helper()
	idx := &ledgerKnowledgeIndex{
		ledger: ledger,
		hits: []knowledge.Hit{
			{ChunkID: originalChunk, Collection: knowledgeScope, Label: "faq.md", Heading: "Refunds", Ordinal: 1, Content: originalKnowledge + " " + strings.Repeat("a", 5000)},
			{ChunkID: "kbc_extra_1", Collection: knowledgeScope, Label: "faq.md", Heading: "Refunds", Ordinal: 2, Content: strings.Repeat("b", 2000)},
			{ChunkID: "kbc_extra_2", Collection: knowledgeScope, Label: "faq.md", Heading: "Refunds", Ordinal: 3, Content: strings.Repeat("c", 2000)},
		},
	}
	if ledger.life > 1 {
		idx = &ledgerKnowledgeIndex{
			ledger: ledger,
			hits:   []knowledge.Hit{{ChunkID: "kbc_changed", Collection: knowledgeScope, Label: "faq.md", Heading: "Refunds", Ordinal: 1, Content: changedKnowledge}},
			err:    errors.New("index unavailable after the restart"),
		}
	}
	return lifeWiring{knowledge: newLedgerKnowledgeProvider(t, idx)}
}

func failingKnowledge(t *testing.T, _ *sql.DB, ledger *turnLedger) lifeWiring {
	t.Helper()
	return lifeWiring{knowledge: newLedgerKnowledgeProvider(t, &ledgerKnowledgeIndex{ledger: ledger, err: errors.New("index unavailable")})}
}

type emptyVault struct{}

func (emptyVault) Contains(string) bool { return false }

type emptyVaultSource struct{}

func (emptyVaultSource) Snapshot(context.Context) (knowledge.SecretScreen, error) {
	return emptyVault{}, nil
}

type ledgerLexical struct {
	ledger *turnLedger
}

func (l *ledgerLexical) ScopeChunks(context.Context, []string) (int, error) { return 10, nil }

func (l *ledgerLexical) TermFrequency(context.Context, []string, string, int) (int, error) {
	return 1, nil
}

func (l *ledgerLexical) Search(context.Context, []string, []string, int) ([]knowledge.Hit, error) {
	if _, err := l.ledger.record(lexicalSearches, ""); err != nil {
		return nil, err
	}
	return []knowledge.Hit{{ChunkID: "kbc_lexical", Collection: knowledgeScope, Label: "faq.md", Ordinal: 1, Content: lexicalKnowledge}}, nil
}

type brokenAfterRestart struct {
	ledger *turnLedger
}

func (b brokenAfterRestart) broken() bool { return b.ledger.life > 1 }

type ledgerVectorView struct {
	brokenAfterRestart
}

func (ledgerVectorView) Len() int { return 1 }

func (v ledgerVectorView) Search(context.Context, []float32, int) ([]knowledge.Hit, error) {
	if v.broken() {
		return nil, errors.New("vector index unavailable after the restart")
	}
	return []knowledge.Hit{{ChunkID: "kbc_semantic", Collection: knowledgeScope, Label: "faq.md", Ordinal: 2, Content: semanticKnowledge}}, nil
}

type ledgerVectorIndex struct {
	view ledgerVectorView
}

func (i ledgerVectorIndex) Current(context.Context, []string) (knowledge.VectorView, error) {
	return i.view, nil
}

type ledgerEmbedder struct {
	brokenAfterRestart
}

func (e ledgerEmbedder) Embed(context.Context, knowledge.EmbedRole, []string) ([][]float32, error) {
	if _, err := e.ledger.record(embeddingCalls, ""); err != nil {
		return nil, err
	}
	if e.broken() {
		return nil, errors.New("embedding provider unavailable after the restart")
	}
	return [][]float32{{1, 0, 0, 0}}, nil
}

func embeddingAcrossLives(t *testing.T, _ *sql.DB, ledger *turnLedger) lifeWiring {
	t.Helper()
	broken := brokenAfterRestart{ledger: ledger}
	retriever, err := knowledge.NewRetriever(&ledgerLexical{ledger: ledger}, emptyVaultSource{}, knowledge.WithVectors(&knowledge.VectorConfig{
		Index:    ledgerVectorIndex{view: ledgerVectorView{broken}},
		Embedder: ledgerEmbedder{broken},
		Settings: &knowledge.EmbeddingSettings{Fingerprint: "fp", Destination: "api.example.com", Model: "m", Dimensions: 4, BatchSize: 96, MaxMemory: 1 << 20},
	}))
	if err != nil {
		t.Fatalf("NewRetriever: %v", err)
	}
	provider, err := knowledge.NewContextProvider(retriever, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("NewContextProvider: %v", err)
	}
	return lifeWiring{knowledge: provider}
}
