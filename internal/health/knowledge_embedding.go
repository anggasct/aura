package health

import (
	"context"
	"fmt"
	"time"
)

const (
	ComponentKnowledgeEmbedding = "knowledge_embedding"

	RemediationEmbedKnowledge = "embed-knowledge-collections"
)

type KnowledgeEmbeddingCollection struct {
	Collection string
	Chunks     int
	Covered    int
	Stale      bool
}

type KnowledgeEmbeddingState struct {
	Configured  bool
	Available   bool
	Collections []KnowledgeEmbeddingCollection
}

type KnowledgeEmbeddingChecker struct {
	State func(ctx context.Context) (KnowledgeEmbeddingState, error)
}

func (c KnowledgeEmbeddingChecker) Check(ctx context.Context) []Finding {
	now := time.Now().UTC()
	if c.State == nil {
		return []Finding{{Component: ComponentKnowledgeEmbedding, Code: "knowledge_embedding_unknown", Status: StatusUnknown, Detail: "knowledge embedding state is not wired", CheckedAt: now}}
	}
	state, err := c.State(ctx)
	if err != nil {
		return []Finding{{Component: ComponentKnowledgeEmbedding, Code: "knowledge_embedding_unknown", Status: StatusUnknown, Detail: "knowledge embedding state unavailable", CheckedAt: now}}
	}
	if len(state.Collections) == 0 {
		return []Finding{{Component: ComponentKnowledgeEmbedding, Code: "knowledge_lexical_only", Status: StatusUp, Detail: lexicalOnlyDetail(&state), CheckedAt: now}}
	}
	findings := make([]Finding, 0, len(state.Collections))
	for i := range state.Collections {
		findings = append(findings, collectionEmbeddingFinding(&state, &state.Collections[i], now))
	}
	return findings
}

func lexicalOnlyDetail(state *KnowledgeEmbeddingState) string {
	switch {
	case !state.Configured:
		return "embedding is not configured; retrieval is lexical only"
	case !state.Available:
		return "embedding credential could not be resolved; retrieval is lexical only"
	default:
		return "embedding is configured but no collection has opted in; retrieval is lexical only"
	}
}

func collectionEmbeddingFinding(state *KnowledgeEmbeddingState, collection *KnowledgeEmbeddingCollection, now time.Time) Finding {
	finding := Finding{
		Component: ComponentKnowledgeEmbedding, Scope: collection.Collection, CheckedAt: now,
		Remediation: RemediationEmbedKnowledge,
	}
	code, status, detail := "ok", StatusUp, fmt.Sprintf("collection %s has vectors for all %d chunks", collection.Collection, collection.Chunks)
	switch {
	case !state.Configured || !state.Available:
		code, status = "kb_embedding_unavailable", StatusDegraded
		detail = fmt.Sprintf("collection %s is opted in but embedding is not available; retrieval is lexical only", collection.Collection)
	case collection.Stale:
		code, status = "kb_reembed_required", StatusDegraded
		detail = fmt.Sprintf("collection %s was embedded under another model or destination; retrieval is lexical only until aura kb embed %s --apply runs", collection.Collection, collection.Collection)
	case collection.Covered < collection.Chunks:
		code, status = "partial", StatusDegraded
		detail = fmt.Sprintf("collection %s has vectors for %d of %d chunks; retrieval is lexical only until aura kb embed %s --apply completes", collection.Collection, collection.Covered, collection.Chunks, collection.Collection)
	}
	finding.Code, finding.Status, finding.Detail = code, status, detail
	finding.ID = ComponentKnowledgeEmbedding + "/" + code + "/" + collection.Collection
	if status == StatusUp {
		finding.Remediation = RemediationNone
	}
	return finding
}
