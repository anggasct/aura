//go:build durable

package app

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/anggasct/aura/internal/modules/knowledge"
	"github.com/anggasct/aura/internal/modules/skills"
	"github.com/anggasct/aura/internal/platform/turn"
	runtimeadk "github.com/anggasct/aura/internal/platform/turn/adk"
	runtimeengine "github.com/anggasct/aura/internal/platform/turn/engine"
	"github.com/anggasct/aura/internal/store"
)

const (
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
	name     string
	approval bool
	fails    bool
}

type lifeWiring struct {
	renderer     runtimeengine.SkillRenderer
	skillContext runtimeadk.SkillContextProvider
	knowledge    runtimeadk.KnowledgeContextProvider
}

type turnScenario struct {
	steps     []string
	tools     []stubTool
	gated     string
	requests  []turn.TurnRequest
	wiring    func(t *testing.T, ledger *turnLedger) lifeWiring
	firstLife func(t *testing.T, life *turnLife)
}

var turnScenarios = map[string]turnScenario{
	"tools": {
		steps:    []string{"tool-a", "tool-b", textStep},
		tools:    []stubTool{{name: "tool-a"}, {name: "tool-b"}},
		gated:    "tool-b",
		requests: []turn.TurnRequest{suiteRequest("turn-1", "USER-TEXT-TOOLS")},
	},
	"approval": {
		steps:    []string{"tool-c", textStep},
		tools:    []stubTool{{name: "tool-c", approval: true}},
		requests: []turn.TurnRequest{suiteRequest("turn-1", "USER-TEXT-APPROVAL")},
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
	"lost-lease": {
		steps:    []string{"tool-a", textStep},
		tools:    []stubTool{{name: "tool-a"}},
		gated:    "tool-a",
		requests: []turn.TurnRequest{suiteRequest("turn-1", "USER-TEXT-LOST-LEASE")},
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

func (s *ledgerSkill) SkillContext(context.Context, *turn.TurnRequest) (string, error) {
	if _, err := s.ledger.record(skillContextCalls, ""); err != nil {
		return "", err
	}
	return s.block, nil
}

func skillAcrossLives(_ *testing.T, ledger *turnLedger) lifeWiring {
	stub := &ledgerSkill{ledger: ledger, body: originalSkillBody, block: turn.SkillContextStart + "]\nORIGINAL-BLOCK\n" + turn.SkillContextEnd}
	if ledger.life > 1 {
		stub = &ledgerSkill{ledger: ledger, body: "CHANGED-BODY", block: "CHANGED-BLOCK", err: skills.Errorf(skills.ErrorCodeSkillUnavailable, "skill is quarantined")}
	}
	return lifeWiring{renderer: stub, skillContext: stub}
}

func transientSkillRender(_ *testing.T, ledger *turnLedger) lifeWiring {
	stub := &ledgerSkill{ledger: ledger, block: turn.SkillContextStart + "]\nUNUSED-BLOCK\n" + turn.SkillContextEnd, err: store.Errorf(store.ErrorCodeStorageUnavailable, "read skill")}
	return lifeWiring{renderer: stub, skillContext: stub}
}

type ledgerKnowledge struct {
	ledger   *turnLedger
	evidence turn.KnowledgeEvidence
	err      error
}

func (k *ledgerKnowledge) KnowledgeContext(_ context.Context, req *turn.TurnRequest) (turn.KnowledgeEvidence, error) {
	scope, err := json.Marshal(req.Knowledge)
	if err != nil {
		return turn.KnowledgeEvidence{}, err
	}
	if _, err := k.ledger.record(knowledgeCalls, string(scope)); err != nil {
		return turn.KnowledgeEvidence{}, err
	}
	return k.evidence, k.err
}

func knowledgeAcrossLives(_ *testing.T, ledger *turnLedger) lifeWiring {
	block := turn.KnowledgeContextStart + ": reference passages]\npassage [source=faq.md#1 trust=untrusted_external] heading: \"Refunds\" text: \"" + originalKnowledge + "\"\n" + turn.KnowledgeContextEnd
	stub := &ledgerKnowledge{ledger: ledger, evidence: turn.KnowledgeEvidence{Block: block, ChunkIDs: []string{originalChunk}, Retrieved: knowledgeRetrievals}}
	if ledger.life > 1 {
		stub = &ledgerKnowledge{
			ledger:   ledger,
			evidence: turn.KnowledgeEvidence{Block: turn.KnowledgeContextStart + "]\n" + changedKnowledge + "\n" + turn.KnowledgeContextEnd, ChunkIDs: []string{"kbc_changed"}, Retrieved: 9},
			err:      errors.New("index unavailable after the restart"),
		}
	}
	return lifeWiring{knowledge: stub}
}

func failingKnowledge(_ *testing.T, ledger *turnLedger) lifeWiring {
	return lifeWiring{knowledge: &ledgerKnowledge{ledger: ledger, err: errors.New("index unavailable")}}
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

func embeddingAcrossLives(t *testing.T, ledger *turnLedger) lifeWiring {
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
	provider, err := knowledge.NewContextProvider(retriever)
	if err != nil {
		t.Fatalf("NewContextProvider: %v", err)
	}
	return lifeWiring{knowledge: provider}
}
