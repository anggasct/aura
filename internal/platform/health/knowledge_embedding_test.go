package health

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func embeddingState(state KnowledgeEmbeddingState, err error) func(context.Context) (KnowledgeEmbeddingState, error) {
	return func(context.Context) (KnowledgeEmbeddingState, error) { return state, err }
}

func TestKnowledgeEmbeddingCheckerReportsLexicalOnlyWithoutOptedInCollections(t *testing.T) {
	tests := map[string]struct {
		state  KnowledgeEmbeddingState
		detail string
	}{
		"not configured":          {state: KnowledgeEmbeddingState{}, detail: "not configured"},
		"credential unresolved":   {state: KnowledgeEmbeddingState{Configured: true}, detail: "credential could not be resolved"},
		"configured but not used": {state: KnowledgeEmbeddingState{Configured: true, Available: true}, detail: "no collection has opted in"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			findings := KnowledgeEmbeddingChecker{State: embeddingState(test.state, nil)}.Check(t.Context())
			if len(findings) != 1 || findings[0].Code != "knowledge_lexical_only" || findings[0].Status != StatusUp ||
				!strings.Contains(findings[0].Detail, "lexical only") || !strings.Contains(findings[0].Detail, test.detail) {
				t.Fatalf("findings = %+v", findings)
			}
		})
	}
}

func TestKnowledgeEmbeddingCheckerReportsCoveragePerCollection(t *testing.T) {
	findings := KnowledgeEmbeddingChecker{State: embeddingState(KnowledgeEmbeddingState{
		Configured: true, Available: true,
		Collections: []KnowledgeEmbeddingCollection{
			{Collection: "complete", Chunks: 4, Covered: 4},
			{Collection: "half", Chunks: 4, Covered: 2},
			{Collection: "moved", Chunks: 4, Covered: 0, Stale: true},
		},
	}, nil)}.Check(t.Context())
	byScope := map[string]Finding{}
	for _, f := range findings {
		byScope[f.Scope] = f
	}
	if len(findings) != 3 {
		t.Fatalf("findings = %+v", findings)
	}
	if f := byScope["complete"]; f.Code != "ok" || f.Status != StatusUp || f.Remediation != RemediationNone {
		t.Errorf("complete = %+v", f)
	}
	if f := byScope["half"]; f.Code != "partial" || f.Status != StatusDegraded || !strings.Contains(f.Detail, "2 of 4") || f.Remediation != RemediationEmbedKnowledge {
		t.Errorf("half = %+v", f)
	}
	if f := byScope["moved"]; f.Code != "kb_reembed_required" || f.Status != StatusDegraded || !strings.Contains(f.Detail, "aura kb embed moved --apply") {
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
	for name, state := range map[string]KnowledgeEmbeddingState{
		"no configuration":      {Collections: []KnowledgeEmbeddingCollection{{Collection: "faq", Chunks: 2}}},
		"unresolved credential": {Configured: true, Collections: []KnowledgeEmbeddingCollection{{Collection: "faq", Chunks: 2, Covered: 2}}},
	} {
		t.Run(name, func(t *testing.T) {
			findings := KnowledgeEmbeddingChecker{State: embeddingState(state, nil)}.Check(t.Context())
			if len(findings) != 1 || findings[0].Code != "kb_embedding_unavailable" || findings[0].Status != StatusDegraded {
				t.Fatalf("findings = %+v", findings)
			}
		})
	}
}

func TestKnowledgeEmbeddingCheckerIsUnknownWhenStateFails(t *testing.T) {
	for name, checker := range map[string]KnowledgeEmbeddingChecker{
		"unwired": {},
		"error":   {State: embeddingState(KnowledgeEmbeddingState{}, errors.New("boom"))},
	} {
		t.Run(name, func(t *testing.T) {
			findings := checker.Check(t.Context())
			if len(findings) != 1 || findings[0].Status != StatusUnknown || findings[0].Code != "knowledge_embedding_unknown" {
				t.Fatalf("findings = %+v", findings)
			}
		})
	}
}

func TestKnowledgeEmbeddingCheckerReportsVectorMemoryAgainstTheLimit(t *testing.T) {
	const mebibyte = 1 << 20
	tests := map[string]struct {
		state      KnowledgeEmbeddingState
		wantScopes map[string]string
		wantMemory string
	}{
		"everything fits": {
			state: KnowledgeEmbeddingState{Configured: true, Available: true, MaxMemory: 256 * mebibyte, Collections: []KnowledgeEmbeddingCollection{
				{Collection: "a", Chunks: 2, Covered: 2, Bytes: 100 * mebibyte},
				{Collection: "b", Chunks: 2, Covered: 2, Bytes: 100 * mebibyte},
			}},
			wantScopes: map[string]string{"a": "ok", "b": "ok"},
			wantMemory: "ok",
		},
		"together they exceed the limit": {
			state: KnowledgeEmbeddingState{Configured: true, Available: true, MaxMemory: 256 * mebibyte, Collections: []KnowledgeEmbeddingCollection{
				{Collection: "a", Chunks: 2, Covered: 2, Bytes: 150 * mebibyte},
				{Collection: "b", Chunks: 2, Covered: 2, Bytes: 150 * mebibyte},
			}},
			wantScopes: map[string]string{"a": "ok", "b": "ok"},
			wantMemory: "kb_memory_limit",
		},
		"one collection alone exceeds the limit": {
			state: KnowledgeEmbeddingState{Configured: true, Available: true, MaxMemory: 256 * mebibyte, Collections: []KnowledgeEmbeddingCollection{
				{Collection: "small", Chunks: 2, Covered: 2, Bytes: 10 * mebibyte},
				{Collection: "huge", Chunks: 2, Covered: 2, Bytes: 390 * mebibyte},
			}},
			wantScopes: map[string]string{"small": "ok", "huge": "kb_memory_limit"},
			wantMemory: "kb_memory_limit",
		},
		"partial and stale collections do not count": {
			state: KnowledgeEmbeddingState{Configured: true, Available: true, MaxMemory: 256 * mebibyte, Collections: []KnowledgeEmbeddingCollection{
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
			byScope := map[string]Finding{}
			for _, f := range (KnowledgeEmbeddingChecker{State: embeddingState(test.state, nil)}).Check(t.Context()) {
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
			if test.wantMemory != "ok" && (memory.Status != StatusDegraded || memory.Remediation != RemediationEmbedKnowledge) {
				t.Errorf("an over-limit report is not degraded: %+v", memory)
			}
		})
	}
}

func TestKnowledgeEmbeddingCheckerReportsNoMemoryLineWithoutReadyCollections(t *testing.T) {
	findings := KnowledgeEmbeddingChecker{State: embeddingState(KnowledgeEmbeddingState{
		Configured: true, Available: true, MaxMemory: 1 << 20,
		Collections: []KnowledgeEmbeddingCollection{{Collection: "half", Chunks: 2, Covered: 1, Bytes: 5}},
	}, nil)}.Check(t.Context())
	for _, f := range findings {
		if f.Scope == "vector_memory" {
			t.Errorf("unexpected memory finding %+v", f)
		}
	}
}
