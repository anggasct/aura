package knowledge

import (
	"context"

	"github.com/anggasct/aura/internal/runtime"
	"github.com/anggasct/aura/internal/runtime/ingress"
)

type ContextProvider struct {
	retriever *Retriever
}

func NewContextProvider(retriever *Retriever) (*ContextProvider, error) {
	if retriever == nil {
		return nil, Errorf(ErrorCodeInvalidArgument, "retriever must not be nil")
	}
	return &ContextProvider{retriever: retriever}, nil
}

func (p *ContextProvider) KnowledgeContext(ctx context.Context, req *runtime.TurnRequest) (runtime.KnowledgeEvidence, error) {
	if req == nil {
		return runtime.KnowledgeEvidence{}, Errorf(ErrorCodeInvalidArgument, "turn request must not be nil")
	}
	if len(req.Knowledge) == 0 {
		return runtime.KnowledgeEvidence{}, nil
	}
	query := runtimeingress.JoinedText(req.Parts)
	if query == "" {
		return runtime.KnowledgeEvidence{}, nil
	}
	found, err := p.retriever.Search(ctx, &SearchRequest{Scope: req.Knowledge, Query: query, Depth: SearchDepth, Class: req.PrincipalClass})
	if err != nil {
		return runtime.KnowledgeEvidence{}, err
	}
	rendered := RenderContext(SelectContext(found.Hits))
	return runtime.KnowledgeEvidence{Block: rendered.Block, ChunkIDs: rendered.ChunkIDs, Retrieved: len(found.Hits), Mode: found.Mode}, nil
}
