package health

import (
	"context"
	"fmt"
	"strings"
	"time"
)

const (
	ComponentKnowledge = "knowledge"

	KnowledgeScopeChunkLimit = 200000

	RemediationReviewKnowledge = "review-knowledge-collections"
)

type KnowledgeScope struct {
	Persona string
	Class   string
	Chunks  int
	Unknown []string
}

type KnowledgeChecker struct {
	Scopes func(ctx context.Context) (scopes []KnowledgeScope, indexed bool, err error)
	Limit  int
}

func (c KnowledgeChecker) Check(ctx context.Context) []Finding {
	now := time.Now().UTC()
	if c.Scopes == nil {
		return []Finding{{Component: ComponentKnowledge, Code: "knowledge_unknown", Status: StatusUnknown, Detail: "knowledge state is not wired", CheckedAt: now}}
	}
	scopes, indexed, err := c.Scopes(ctx)
	if err != nil {
		return []Finding{{Component: ComponentKnowledge, Code: "knowledge_unknown", Status: StatusUnknown, Detail: "knowledge state unavailable", CheckedAt: now}}
	}
	if !indexed {
		return []Finding{{Component: ComponentKnowledge, Code: "knowledge_index_missing", Status: StatusDegraded, Detail: "personas list knowledge collections but the database has no knowledge index; run migrations", CheckedAt: now}}
	}
	limit := c.Limit
	if limit <= 0 {
		limit = KnowledgeScopeChunkLimit
	}
	var findings []Finding
	for i := range scopes {
		scope := &scopes[i]
		label := scope.Persona + "/" + scope.Class
		if scope.Chunks > limit {
			findings = append(findings, Finding{
				ID: ComponentKnowledge + "/knowledge_scope_oversized/" + label, Component: ComponentKnowledge, Scope: label,
				Code: "knowledge_scope_oversized", Status: StatusDegraded, CheckedAt: now,
				Detail: fmt.Sprintf("persona %s sees %d chunks as %s, above the %d supported; retrieval may time out and skip a turn", scope.Persona, scope.Chunks, scope.Class, limit),
			})
		}
		if len(scope.Unknown) > 0 {
			findings = append(findings, Finding{
				ID: ComponentKnowledge + "/knowledge_collection_unknown/" + label, Component: ComponentKnowledge, Scope: label,
				Code: "knowledge_collection_unknown", Status: StatusDegraded, CheckedAt: now,
				Detail: fmt.Sprintf("persona %s lists collections for %s that do not exist yet: %s", scope.Persona, scope.Class, strings.Join(scope.Unknown, ", ")),
			})
		}
	}
	if len(findings) == 0 {
		return []Finding{{Component: ComponentKnowledge, Code: "ok", Status: StatusUp, Detail: fmt.Sprintf("%d knowledge scopes within limits", len(scopes)), CheckedAt: now}}
	}
	return findings
}
