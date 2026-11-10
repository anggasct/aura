package knowledge

import (
	"context"
	"strings"
	"time"

	"github.com/anggasct/aura/internal/platform/principal"
)

const (
	QueryEmbedTimeout  = 400 * time.Millisecond
	RetrievalBudget    = 500 * time.Millisecond
	MaxEmbedQueryBytes = 16 << 10
)

type vectorPath struct {
	index    VectorIndex
	embedder Embedder
	settings *EmbeddingSettings
	timeout  time.Duration
}

func (p *vectorPath) allows(class principal.Class) bool {
	switch class {
	case principal.ClassOwner, principal.ClassTrusted:
		return true
	case principal.ClassPublic:
		return p.settings.PublicQueries
	default:
		return false
	}
}

func (p *vectorPath) search(ctx context.Context, collections []string, request *SearchRequest, vault *vaultSnapshot) (hits []Hit, mode string) {
	if p == nil || !p.allows(request.Class) || len(request.Query) > MaxEmbedQueryBytes {
		return nil, ModeLexical
	}
	view, err := p.index.Current(ctx, collections)
	if err != nil || view == nil || view.Len() == 0 {
		return nil, ModeLexical
	}
	text := Normalize(request.Query, FormatText)
	if strings.TrimSpace(text) == "" {
		return nil, ModeLexical
	}
	screen, err := vault.get(ctx)
	if err != nil || screenText(text, screen) != nil {
		return nil, ModeLexical
	}
	embedCtx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	vectors, err := p.embedder.Embed(embedCtx, EmbedRoleQuery, []string{text})
	if err != nil || len(vectors) != 1 {
		return nil, ModeLexicalFallback
	}
	query, ok := normalizeVector(vectors[0], p.settings.Dimensions)
	if !ok {
		return nil, ModeLexicalFallback
	}
	hits, err = view.Search(ctx, query, SearchDepth)
	if err != nil {
		return nil, ModeLexicalFallback
	}
	return hits, ModeHybrid
}
