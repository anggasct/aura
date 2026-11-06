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
