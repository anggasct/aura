package health

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func scopesOf(scopes []KnowledgeScope, indexed bool, err error) func(context.Context) ([]KnowledgeScope, bool, error) {
	return func(context.Context) ([]KnowledgeScope, bool, error) { return scopes, indexed, err }
}

func TestKnowledgeCheckerReportsOversizedScopesAndUnknownCollections(t *testing.T) {
	findings := KnowledgeChecker{Limit: 100, Scopes: scopesOf([]KnowledgeScope{
		{Persona: "lobby", Class: "owner", Chunks: 100},
		{Persona: "lobby", Class: "public", Chunks: 101},
		{Persona: "desk", Class: "trusted", Chunks: 5, Unknown: []string{"faq", "hr-docs"}},
	}, true, nil)}.Check(t.Context())

	if len(findings) != 2 {
		t.Fatalf("findings = %+v, want one oversized and one unknown", findings)
	}
	byCode := map[string]Finding{}
	for _, f := range findings {
		byCode[f.Code] = f
	}
	oversized, unknown := byCode["knowledge_scope_oversized"], byCode["knowledge_collection_unknown"]
	if oversized.Status != StatusDegraded || oversized.Component != ComponentKnowledge || oversized.Scope != "lobby/public" || !strings.Contains(oversized.Detail, "101 chunks") || !strings.Contains(oversized.Detail, "100 supported") {
		t.Errorf("oversized = %+v", oversized)
	}
	if unknown.Status != StatusDegraded || unknown.Scope != "desk/trusted" || !strings.Contains(unknown.Detail, "faq, hr-docs") {
		t.Errorf("unknown = %+v", unknown)
	}
	if oversized.ID == unknown.ID || oversized.ID == "" {
		t.Errorf("ids must differ per finding: %q %q", oversized.ID, unknown.ID)
	}
}

func TestKnowledgeCheckerDefaultsToTheSupportedScopeSize(t *testing.T) {
	within := KnowledgeChecker{Scopes: scopesOf([]KnowledgeScope{{Persona: "a", Class: "owner", Chunks: KnowledgeScopeChunkLimit}}, true, nil)}.Check(t.Context())
	if len(within) != 1 || within[0].Code != "ok" || within[0].Status != StatusUp {
		t.Fatalf("at the limit = %+v", within)
	}
	over := KnowledgeChecker{Scopes: scopesOf([]KnowledgeScope{{Persona: "a", Class: "owner", Chunks: KnowledgeScopeChunkLimit + 1}}, true, nil)}.Check(t.Context())
	if len(over) != 1 || over[0].Code != "knowledge_scope_oversized" {
		t.Fatalf("over the limit = %+v", over)
	}
}

func TestKnowledgeCheckerToleratesAMissingIndexAndFailures(t *testing.T) {
	missing := KnowledgeChecker{Scopes: scopesOf(nil, false, nil)}.Check(t.Context())
	if len(missing) != 1 || missing[0].Code != "knowledge_index_missing" || missing[0].Status != StatusDegraded {
		t.Errorf("missing index = %+v", missing)
	}
	failed := KnowledgeChecker{Scopes: scopesOf(nil, true, errors.New("secret path /var/db"))}.Check(t.Context())
	if len(failed) != 1 || failed[0].Code != "knowledge_unknown" || failed[0].Status != StatusUnknown || strings.Contains(failed[0].Detail, "/var/db") {
		t.Errorf("failure = %+v", failed)
	}
	unwired := KnowledgeChecker{}.Check(t.Context())
	if len(unwired) != 1 || unwired[0].Status != StatusUnknown {
		t.Errorf("unwired = %+v", unwired)
	}
}
