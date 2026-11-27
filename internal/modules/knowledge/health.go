package knowledge

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/anggasct/aura/internal/platform/embedding"
	"github.com/anggasct/aura/internal/platform/health"
	"github.com/anggasct/aura/internal/platform/sqlite"
)

const (
	ComponentKnowledge          = "knowledge"
	ComponentKnowledgeEmbedding = "knowledge_embedding"

	ScopeChunkLimit = 200000

	RemediationReview = "review-knowledge-collections"
	RemediationEmbed  = "embed-knowledge-collections"
)

type PersonaScope struct {
	Persona     string
	Class       string
	Collections []string
}

type ScopeStatus struct {
	Persona string
	Class   string
	Chunks  int
	Unknown []string
}

type ScopeChecker struct {
	Scopes func(ctx context.Context) (scopes []ScopeStatus, indexed bool, err error)
	Limit  int
}

func (c ScopeChecker) Check(ctx context.Context) []health.Finding {
	now := time.Now().UTC()
	if c.Scopes == nil {
		return []health.Finding{{Component: ComponentKnowledge, Code: "knowledge_unknown", Status: health.StatusUnknown, Detail: "knowledge state is not wired", CheckedAt: now}}
	}
	scopes, indexed, err := c.Scopes(ctx)
	if err != nil {
		return []health.Finding{{Component: ComponentKnowledge, Code: "knowledge_unknown", Status: health.StatusUnknown, Detail: "knowledge state unavailable", CheckedAt: now}}
	}
	if !indexed {
		return []health.Finding{{Component: ComponentKnowledge, Code: "knowledge_index_missing", Status: health.StatusDegraded, Detail: "personas list knowledge collections but the database has no knowledge index; run migrations", CheckedAt: now}}
	}
	limit := c.Limit
	if limit <= 0 {
		limit = ScopeChunkLimit
	}
	var findings []health.Finding
	for i := range scopes {
		scope := &scopes[i]
		label := scope.Persona + "/" + scope.Class
		if scope.Chunks > limit {
			findings = append(findings, health.Finding{
				ID: ComponentKnowledge + "/knowledge_scope_oversized/" + label, Component: ComponentKnowledge, Scope: label,
				Code: "knowledge_scope_oversized", Status: health.StatusDegraded, CheckedAt: now,
				Detail: fmt.Sprintf("persona %s sees %d chunks as %s, above the %d supported; retrieval may time out and skip a turn", scope.Persona, scope.Chunks, scope.Class, limit),
			})
		}
		if len(scope.Unknown) > 0 {
			findings = append(findings, health.Finding{
				ID: ComponentKnowledge + "/knowledge_collection_unknown/" + label, Component: ComponentKnowledge, Scope: label,
				Code: "knowledge_collection_unknown", Status: health.StatusDegraded, CheckedAt: now,
				Detail: fmt.Sprintf("persona %s lists collections for %s that do not exist yet: %s", scope.Persona, scope.Class, strings.Join(scope.Unknown, ", ")),
			})
		}
	}
	if len(findings) == 0 {
		return []health.Finding{{Component: ComponentKnowledge, Code: "ok", Status: health.StatusUp, Detail: fmt.Sprintf("%d knowledge scopes within limits", len(scopes)), CheckedAt: now}}
	}
	return findings
}

type EmbeddingCollectionStatus struct {
	Collection string
	Chunks     int
	Covered    int
	Stale      bool
	Bytes      int64
}

type EmbeddingHealthState struct {
	Configured  bool
	Available   bool
	MaxMemory   int64
	Collections []EmbeddingCollectionStatus
}

type EmbeddingChecker struct {
	State func(ctx context.Context) (EmbeddingHealthState, error)
}

func (c EmbeddingChecker) Check(ctx context.Context) []health.Finding {
	now := time.Now().UTC()
	if c.State == nil {
		return []health.Finding{{Component: ComponentKnowledgeEmbedding, Code: "knowledge_embedding_unknown", Status: health.StatusUnknown, Detail: "knowledge embedding state is not wired", CheckedAt: now}}
	}
	state, err := c.State(ctx)
	if err != nil {
		return []health.Finding{{Component: ComponentKnowledgeEmbedding, Code: "knowledge_embedding_unknown", Status: health.StatusUnknown, Detail: "knowledge embedding state unavailable", CheckedAt: now}}
	}
	if len(state.Collections) == 0 {
		return []health.Finding{{Component: ComponentKnowledgeEmbedding, Code: "knowledge_lexical_only", Status: health.StatusUp, Detail: lexicalOnlyDetail(&state), CheckedAt: now}}
	}
	findings := make([]health.Finding, 0, len(state.Collections)+1)
	for i := range state.Collections {
		findings = append(findings, collectionEmbeddingFinding(&state, &state.Collections[i], now))
	}
	if memory, ok := vectorMemoryFinding(&state, now); ok {
		findings = append(findings, memory)
	}
	return findings
}

func (c *EmbeddingCollectionStatus) ready() bool {
	return !c.Stale && c.Chunks > 0 && c.Covered == c.Chunks
}

func vectorMemoryFinding(state *EmbeddingHealthState, now time.Time) (health.Finding, bool) {
	if !state.Configured || !state.Available || state.MaxMemory <= 0 {
		return health.Finding{}, false
	}
	var total int64
	ready := 0
	for i := range state.Collections {
		if state.Collections[i].ready() {
			total += state.Collections[i].Bytes
			ready++
		}
	}
	if ready == 0 {
		return health.Finding{}, false
	}
	finding := health.Finding{
		Component: ComponentKnowledgeEmbedding, Scope: "vector_memory", CheckedAt: now, Remediation: health.RemediationNone,
		Code: "ok", Status: health.StatusUp, ID: ComponentKnowledgeEmbedding + "/ok/vector_memory",
		Detail: fmt.Sprintf("vectors for %d ready collections need %s of the %s max_memory", ready, healthByteSize(total), healthByteSize(state.MaxMemory)),
	}
	if total > state.MaxMemory {
		finding.Code, finding.Status, finding.Remediation = "kb_memory_limit", health.StatusDegraded, RemediationEmbed
		finding.ID = ComponentKnowledgeEmbedding + "/kb_memory_limit/vector_memory"
		finding.Detail = fmt.Sprintf("vectors for %d ready collections need %s, above the %s max_memory; collections that do not fit stay lexical only until knowledge.embedding.max_memory is raised", ready, healthByteSize(total), healthByteSize(state.MaxMemory))
	}
	return finding, true
}

func healthByteSize(bytes int64) string {
	const mebibyte = 1 << 20
	return fmt.Sprintf("%.1f MiB", float64(bytes)/mebibyte)
}

func lexicalOnlyDetail(state *EmbeddingHealthState) string {
	switch {
	case !state.Configured:
		return "embedding is not configured; retrieval is lexical only"
	case !state.Available:
		return "embedding credential could not be resolved; retrieval is lexical only"
	default:
		return "embedding is configured but no collection has opted in; retrieval is lexical only"
	}
}

func collectionEmbeddingFinding(state *EmbeddingHealthState, collection *EmbeddingCollectionStatus, now time.Time) health.Finding {
	finding := health.Finding{
		Component: ComponentKnowledgeEmbedding, Scope: collection.Collection, CheckedAt: now,
		Remediation: RemediationEmbed,
	}
	code, status, detail := "ok", health.StatusUp, fmt.Sprintf("collection %s has vectors for all %d chunks", collection.Collection, collection.Chunks)
	switch {
	case !state.Configured || !state.Available:
		code, status = "kb_embedding_unavailable", health.StatusDegraded
		detail = fmt.Sprintf("collection %s is opted in but embedding is not available; retrieval is lexical only", collection.Collection)
	case collection.Stale:
		code, status = "kb_reembed_required", health.StatusDegraded
		detail = fmt.Sprintf("collection %s was embedded under another model or destination; retrieval is lexical only until aura kb embed %s --apply runs", collection.Collection, collection.Collection)
	case collection.ready() && state.MaxMemory > 0 && collection.Bytes > state.MaxMemory:
		code, status = "kb_memory_limit", health.StatusDegraded
		detail = fmt.Sprintf("collection %s needs %s of vectors, above the %s max_memory; retrieval is lexical only for it until knowledge.embedding.max_memory is raised", collection.Collection, healthByteSize(collection.Bytes), healthByteSize(state.MaxMemory))
	case collection.Covered < collection.Chunks:
		code, status = "partial", health.StatusDegraded
		detail = fmt.Sprintf("collection %s has vectors for %d of %d chunks; retrieval is lexical only until aura kb embed %s --apply completes", collection.Collection, collection.Covered, collection.Chunks, collection.Collection)
	}
	finding.Code, finding.Status, finding.Detail = code, status, detail
	finding.ID = ComponentKnowledgeEmbedding + "/" + code + "/" + collection.Collection
	if status == health.StatusUp {
		finding.Remediation = health.RemediationNone
	}
	return finding
}

func HealthChecks(cfg *Config, scopes []PersonaScope, dbPath string, timeout time.Duration) []health.RegisteredCheck {
	var checks []health.RegisteredCheck
	if len(scopes) > 0 {
		checks = append(checks, health.RegisteredCheck{
			ID:          ComponentKnowledge,
			Checker:     ScopeChecker{Scopes: scopeReader(scopes, dbPath), Limit: ScopeChunkLimit},
			Timeout:     timeout,
			Remediation: RemediationReview,
		})
	}
	if len(scopes) > 0 || (cfg != nil && cfg.Embedding != nil) {
		checks = append(checks, health.RegisteredCheck{
			ID:          ComponentKnowledgeEmbedding,
			Checker:     EmbeddingChecker{State: embeddingHealthReader(cfg, dbPath)},
			Timeout:     timeout,
			Remediation: RemediationEmbed,
		})
	}
	return checks
}

func scopeReader(configured []PersonaScope, dbPath string) func(ctx context.Context) ([]ScopeStatus, bool, error) {
	cloned := make([]PersonaScope, len(configured))
	for i := range configured {
		cloned[i] = PersonaScope{
			Persona:     configured[i].Persona,
			Class:       configured[i].Class,
			Collections: slices.Clone(configured[i].Collections),
		}
	}
	return func(ctx context.Context) ([]ScopeStatus, bool, error) {
		if _, err := os.Stat(dbPath); err != nil {
			return nil, false, err
		}
		db, err := sqlite.OpenReadOnly(ctx, dbPath)
		if err != nil {
			return nil, false, err
		}
		defer func() { _ = db.Close() }()
		present, err := IndexPresent(ctx, db)
		if err != nil || !present {
			return nil, present, err
		}
		kbStore, err := NewSQLiteStore(db)
		if err != nil {
			return nil, true, err
		}
		collections, err := kbStore.ListCollections(ctx)
		if err != nil {
			return nil, true, err
		}
		sizes := make(map[string]int, len(collections))
		for i := range collections {
			sizes[collections[i].ID] = collections[i].Chunks
		}
		scopes := make([]ScopeStatus, 0, len(cloned))
		for i := range cloned {
			scope := &cloned[i]
			status := ScopeStatus{Persona: scope.Persona, Class: scope.Class}
			for _, id := range scope.Collections {
				size, known := sizes[id]
				if !known {
					status.Unknown = append(status.Unknown, id)
					continue
				}
				status.Chunks += size
			}
			slices.Sort(status.Unknown)
			scopes = append(scopes, status)
		}
		return scopes, true, nil
	}
}

func embeddingHealthReader(cfg *Config, dbPath string) func(ctx context.Context) (EmbeddingHealthState, error) {
	return func(ctx context.Context) (EmbeddingHealthState, error) {
		var state EmbeddingHealthState
		fingerprint := ""
		dimensions := 0
		if cfg != nil && cfg.Embedding != nil {
			embeddingConfig := cfg.Embedding
			clientCfg := embeddingConfig.ClientConfig()
			computed, err := embedding.Fingerprint(&clientCfg)
			if err != nil {
				return EmbeddingHealthState{}, err
			}
			fingerprint = computed
			_, state.Available = ResolveEmbeddingCredential(embeddingConfig.CredentialRef)
			state.Configured = true
			state.MaxMemory = int64(embeddingConfig.MaxMemory)
			dimensions = embeddingConfig.Dimensions
		}
		if _, err := os.Stat(dbPath); err != nil {
			return EmbeddingHealthState{}, err
		}
		db, err := sqlite.OpenReadOnly(ctx, dbPath)
		if err != nil {
			return EmbeddingHealthState{}, err
		}
		defer func() { _ = db.Close() }()
		present, err := EmbeddingPresent(ctx, db)
		if err != nil || !present {
			return state, err
		}
		kbStore, err := NewSQLiteStore(db)
		if err != nil {
			return EmbeddingHealthState{}, err
		}
		coverage, err := kbStore.EmbeddingCoverage(ctx, fingerprint)
		if err != nil {
			return EmbeddingHealthState{}, err
		}
		for i := range coverage {
			state.Collections = append(state.Collections, EmbeddingCollectionStatus{
				Collection: coverage[i].Collection, Chunks: coverage[i].Chunks, Covered: coverage[i].Covered,
				Stale: state.Configured && coverage[i].Fingerprint != fingerprint,
				Bytes: VectorBytes(coverage[i].Chunks, dimensions),
			})
		}
		return state, nil
	}
}
