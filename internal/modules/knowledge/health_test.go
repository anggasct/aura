package knowledge

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/health"
)

func scopesOf(scopes []ScopeStatus, indexed bool, err error) func(context.Context) ([]ScopeStatus, bool, error) {
	return func(context.Context) ([]ScopeStatus, bool, error) { return scopes, indexed, err }
}

func embeddingState(state EmbeddingHealthState, err error) func(context.Context) (EmbeddingHealthState, error) {
	return func(context.Context) (EmbeddingHealthState, error) { return state, err }
}

func TestKnowledgeCheckerReportsOversizedScopesAndUnknownCollections(t *testing.T) {
	findings := ScopeChecker{Limit: 100, Scopes: scopesOf([]ScopeStatus{
		{Persona: "lobby", Class: "owner", Chunks: 100},
		{Persona: "lobby", Class: "public", Chunks: 101},
		{Persona: "desk", Class: "trusted", Chunks: 5, Unknown: []string{"faq", "hr-docs"}},
	}, true, nil)}.Check(t.Context())

	if len(findings) != 2 {
		t.Fatalf("findings = %+v, want one oversized and one unknown", findings)
	}
	byCode := map[string]health.Finding{}
	for _, f := range findings {
		byCode[f.Code] = f
	}
	oversized, unknown := byCode["knowledge_scope_oversized"], byCode["knowledge_collection_unknown"]
	if oversized.Status != health.StatusDegraded || oversized.Component != ComponentKnowledge || oversized.Scope != "lobby/public" || !strings.Contains(oversized.Detail, "101 chunks") || !strings.Contains(oversized.Detail, "100 supported") {
		t.Errorf("oversized = %+v", oversized)
	}
	if unknown.Status != health.StatusDegraded || unknown.Scope != "desk/trusted" || !strings.Contains(unknown.Detail, "faq, hr-docs") {
		t.Errorf("unknown = %+v", unknown)
	}
	if oversized.ID == unknown.ID || oversized.ID == "" {
		t.Errorf("ids must differ per finding: %q %q", oversized.ID, unknown.ID)
	}
}

func TestKnowledgeCheckerDefaultsToTheSupportedScopeSize(t *testing.T) {
	within := ScopeChecker{Scopes: scopesOf([]ScopeStatus{{Persona: "a", Class: "owner", Chunks: ScopeChunkLimit}}, true, nil)}.Check(t.Context())
	if len(within) != 1 || within[0].Code != "ok" || within[0].Status != health.StatusUp {
		t.Fatalf("at the limit = %+v", within)
	}
	over := ScopeChecker{Scopes: scopesOf([]ScopeStatus{{Persona: "a", Class: "owner", Chunks: ScopeChunkLimit + 1}}, true, nil)}.Check(t.Context())
	if len(over) != 1 || over[0].Code != "knowledge_scope_oversized" {
		t.Fatalf("over the limit = %+v", over)
	}
}

func TestKnowledgeCheckerToleratesAMissingIndexAndFailures(t *testing.T) {
	missing := ScopeChecker{Scopes: scopesOf(nil, false, nil)}.Check(t.Context())
	if len(missing) != 1 || missing[0].Code != "knowledge_index_missing" || missing[0].Status != health.StatusDegraded {
		t.Errorf("missing index = %+v", missing)
	}
	failed := ScopeChecker{Scopes: scopesOf(nil, true, errors.New("secret path /var/db"))}.Check(t.Context())
	if len(failed) != 1 || failed[0].Code != "knowledge_unknown" || failed[0].Status != health.StatusUnknown || strings.Contains(failed[0].Detail, "/var/db") {
		t.Errorf("failure = %+v", failed)
	}
	unwired := ScopeChecker{}.Check(t.Context())
	if len(unwired) != 1 || unwired[0].Status != health.StatusUnknown {
		t.Errorf("unwired = %+v", unwired)
	}
}

func TestKnowledgeEmbeddingCheckerReportsLexicalOnlyWithoutOptedInCollections(t *testing.T) {
	tests := map[string]struct {
		state  EmbeddingHealthState
		detail string
	}{
		"not configured":          {state: EmbeddingHealthState{}, detail: "not configured"},
		"credential unresolved":   {state: EmbeddingHealthState{Configured: true}, detail: "credential could not be resolved"},
		"configured but not used": {state: EmbeddingHealthState{Configured: true, Available: true}, detail: "no collection has opted in"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			findings := EmbeddingChecker{State: embeddingState(test.state, nil)}.Check(t.Context())
			if len(findings) != 1 || findings[0].Code != "knowledge_lexical_only" || findings[0].Status != health.StatusUp ||
				!strings.Contains(findings[0].Detail, "lexical only") || !strings.Contains(findings[0].Detail, test.detail) {
				t.Fatalf("findings = %+v", findings)
			}
		})
	}
}

func TestKnowledgeEmbeddingCheckerReportsCoveragePerCollection(t *testing.T) {
	findings := EmbeddingChecker{State: embeddingState(EmbeddingHealthState{
		Configured: true, Available: true,
		Collections: []EmbeddingCollectionStatus{
			{Collection: "complete", Chunks: 4, Covered: 4},
			{Collection: "half", Chunks: 4, Covered: 2},
			{Collection: "moved", Chunks: 4, Covered: 0, Stale: true},
		},
	}, nil)}.Check(t.Context())
	byScope := map[string]health.Finding{}
	for _, f := range findings {
		byScope[f.Scope] = f
	}
	if len(findings) != 3 {
		t.Fatalf("findings = %+v", findings)
	}
	if f := byScope["complete"]; f.Code != "ok" || f.Status != health.StatusUp || f.Remediation != health.RemediationNone {
		t.Errorf("complete = %+v", f)
	}
	if f := byScope["half"]; f.Code != "partial" || f.Status != health.StatusDegraded || !strings.Contains(f.Detail, "2 of 4") || f.Remediation != RemediationEmbed {
		t.Errorf("half = %+v", f)
	}
	if f := byScope["moved"]; f.Code != "kb_reembed_required" || f.Status != health.StatusDegraded || !strings.Contains(f.Detail, "aura kb embed moved --apply") {
		t.Errorf("moved = %+v", f)
	}
	seen := map[string]bool{}
	for _, f := range findings {
		if f.ID == "" || seen[f.ID] {
			t.Errorf("finding ids must be unique and set: %q", f.ID)
		}
		seen[f.ID] = true
	}
}

func TestKnowledgeEmbeddingCheckerReportsOptedInCollectionsWithoutAnAvailableEmbedder(t *testing.T) {
	for name, state := range map[string]EmbeddingHealthState{
		"no configuration":      {Collections: []EmbeddingCollectionStatus{{Collection: "faq", Chunks: 2}}},
		"unresolved credential": {Configured: true, Collections: []EmbeddingCollectionStatus{{Collection: "faq", Chunks: 2, Covered: 2}}},
	} {
		t.Run(name, func(t *testing.T) {
			findings := EmbeddingChecker{State: embeddingState(state, nil)}.Check(t.Context())
			if len(findings) != 1 || findings[0].Code != "kb_embedding_unavailable" || findings[0].Status != health.StatusDegraded {
				t.Fatalf("findings = %+v", findings)
			}
		})
	}
}

func TestKnowledgeEmbeddingCheckerIsUnknownWhenStateFails(t *testing.T) {
	for name, checker := range map[string]EmbeddingChecker{
		"unwired": {},
		"error":   {State: embeddingState(EmbeddingHealthState{}, errors.New("boom"))},
	} {
		t.Run(name, func(t *testing.T) {
			findings := checker.Check(t.Context())
			if len(findings) != 1 || findings[0].Status != health.StatusUnknown || findings[0].Code != "knowledge_embedding_unknown" {
				t.Fatalf("findings = %+v", findings)
			}
		})
	}
}

func TestKnowledgeEmbeddingCheckerReportsVectorMemoryAgainstTheLimit(t *testing.T) {
	const mebibyte = 1 << 20
	tests := map[string]struct {
		state      EmbeddingHealthState
		wantScopes map[string]string
		wantMemory string
	}{
		"everything fits": {
			state: EmbeddingHealthState{Configured: true, Available: true, MaxMemory: 256 * mebibyte, Collections: []EmbeddingCollectionStatus{
				{Collection: "a", Chunks: 2, Covered: 2, Bytes: 100 * mebibyte},
				{Collection: "b", Chunks: 2, Covered: 2, Bytes: 100 * mebibyte},
			}},
			wantScopes: map[string]string{"a": "ok", "b": "ok"},
			wantMemory: "ok",
		},
		"together they exceed the limit": {
			state: EmbeddingHealthState{Configured: true, Available: true, MaxMemory: 256 * mebibyte, Collections: []EmbeddingCollectionStatus{
				{Collection: "a", Chunks: 2, Covered: 2, Bytes: 150 * mebibyte},
				{Collection: "b", Chunks: 2, Covered: 2, Bytes: 150 * mebibyte},
			}},
			wantScopes: map[string]string{"a": "ok", "b": "ok"},
			wantMemory: "kb_memory_limit",
		},
		"one collection alone exceeds the limit": {
			state: EmbeddingHealthState{Configured: true, Available: true, MaxMemory: 256 * mebibyte, Collections: []EmbeddingCollectionStatus{
				{Collection: "small", Chunks: 2, Covered: 2, Bytes: 10 * mebibyte},
				{Collection: "huge", Chunks: 2, Covered: 2, Bytes: 390 * mebibyte},
			}},
			wantScopes: map[string]string{"small": "ok", "huge": "kb_memory_limit"},
			wantMemory: "kb_memory_limit",
		},
		"partial and stale collections do not count": {
			state: EmbeddingHealthState{Configured: true, Available: true, MaxMemory: 256 * mebibyte, Collections: []EmbeddingCollectionStatus{
				{Collection: "ready", Chunks: 2, Covered: 2, Bytes: 10 * mebibyte},
				{Collection: "half", Chunks: 2, Covered: 1, Bytes: 900 * mebibyte},
				{Collection: "moved", Chunks: 2, Covered: 2, Stale: true, Bytes: 900 * mebibyte},
			}},
			wantScopes: map[string]string{"ready": "ok", "half": "partial", "moved": "kb_reembed_required"},
			wantMemory: "ok",
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			byScope := map[string]health.Finding{}
			for _, f := range (EmbeddingChecker{State: embeddingState(test.state, nil)}).Check(t.Context()) {
				byScope[f.Scope] = f
			}
			for scope, code := range test.wantScopes {
				if byScope[scope].Code != code {
					t.Errorf("%s = %+v, want code %s", scope, byScope[scope], code)
				}
			}
			memory := byScope["vector_memory"]
			if memory.Code != test.wantMemory || !strings.Contains(memory.Detail, "max_memory") {
				t.Errorf("vector_memory = %+v, want code %s", memory, test.wantMemory)
			}
			if test.wantMemory != "ok" && (memory.Status != health.StatusDegraded || memory.Remediation != RemediationEmbed) {
				t.Errorf("an over-limit report is not degraded: %+v", memory)
			}
		})
	}
}

func TestKnowledgeEmbeddingCheckerReportsNoMemoryLineWithoutReadyCollections(t *testing.T) {
	findings := EmbeddingChecker{State: embeddingState(EmbeddingHealthState{
		Configured: true, Available: true, MaxMemory: 1 << 20,
		Collections: []EmbeddingCollectionStatus{{Collection: "half", Chunks: 2, Covered: 1, Bytes: 5}},
	}, nil)}.Check(t.Context())
	for _, f := range findings {
		if f.Scope == "vector_memory" {
			t.Errorf("unexpected memory finding %+v", f)
		}
	}
}

func TestHealthChecksIncludesEmbeddingWithoutPersonaScopes(t *testing.T) {
	if checks := HealthChecks(nil, nil, "/tmp/none.db", time.Second); len(checks) != 0 {
		t.Fatalf("empty config and scopes produced %d checks, want 0", len(checks))
	}
	cfg := &Config{Embedding: &EmbeddingConfig{BaseURL: "https://api.example.com/v1", Model: "m", Dimensions: 4}}
	checks := HealthChecks(cfg, nil, "/tmp/none.db", time.Second)
	if len(checks) != 1 || checks[0].ID != ComponentKnowledgeEmbedding {
		t.Fatalf("embedding-only checks = %+v, want [%s]", checks, ComponentKnowledgeEmbedding)
	}
	withScopes := HealthChecks(nil, []PersonaScope{{Persona: "lobby", Class: "owner", Collections: []string{"faq"}}}, "/tmp/none.db", time.Second)
	if len(withScopes) != 2 || withScopes[0].ID != ComponentKnowledge || withScopes[1].ID != ComponentKnowledgeEmbedding {
		t.Fatalf("scoped checks = %+v, want [%s, %s]", withScopes, ComponentKnowledge, ComponentKnowledgeEmbedding)
	}
}
