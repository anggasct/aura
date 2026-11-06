package toolsbuiltin

import (
	"context"
	"encoding/json"
	"sync/atomic"

	"github.com/anggasct/aura/internal/approval"
	"github.com/anggasct/aura/internal/principal"
	"github.com/anggasct/aura/internal/toolbroker"
	"github.com/anggasct/aura/internal/tools"
)

const defaultKnowledgeSearchLimit = 5

var emptyKnowledgeSearch = json.RawMessage(`{"results":[],"truncated":false,"mode":"lexical"}`)

type KnowledgeSearcher interface {
	SearchKnowledge(ctx context.Context, scope []string, query string, limit int, class principal.Class) (json.RawMessage, error)
}

type knowledgeHolder struct {
	searcher atomic.Pointer[KnowledgeSearcher]
}

func (h *knowledgeHolder) adapter(ctx context.Context, request *toolbroker.ToolRequest, _ approval.Constraints) (toolbroker.ToolResult, error) {
	if request == nil {
		return toolbroker.ToolResult{}, toolbroker.Errorf(toolbroker.ResultInvalidArgument, "request must not be nil")
	}
	var args struct {
		Query string `json:"query"`
		Limit *int   `json:"limit"`
	}
	if err := json.Unmarshal(request.Arguments, &args); err != nil || args.Query == "" {
		return toolbroker.ToolResult{}, toolbroker.Errorf(toolbroker.ResultInvalidArgument, "query must be a non-empty string")
	}
	if len(request.Knowledge) == 0 {
		return toolbroker.ToolResult{Class: toolbroker.ResultOK, Output: emptyKnowledgeSearch}, nil
	}
	searcher := h.searcher.Load()
	if searcher == nil {
		return toolbroker.ToolResult{}, toolbroker.Errorf(toolbroker.ResultCapabilityUnavailable, "knowledge search is not available")
	}
	limit := defaultKnowledgeSearchLimit
	if args.Limit != nil {
		limit = min(max(*args.Limit, 1), tools.MaxKBSearchResultLimit)
	}
	output, err := (*searcher).SearchKnowledge(ctx, request.Knowledge, args.Query, limit, request.PrincipalClass)
	if err != nil {
		return toolbroker.ToolResult{}, toolbroker.Errorf(toolbroker.ResultCapabilityUnavailable, "knowledge search failed")
	}
	return toolbroker.ToolResult{Class: toolbroker.ResultOK, Output: output}, nil
}

func (e *Executor) SetKnowledgeSearcher(searcher KnowledgeSearcher) {
	if e == nil || e.knowledge == nil || searcher == nil {
		return
	}
	e.knowledge.searcher.Store(&searcher)
}
