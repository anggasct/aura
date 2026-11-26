package knowledge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"time"
	"unicode/utf8"

	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/toolbroker"
	"github.com/anggasct/aura/internal/platform/tools"
)

var emptyKnowledgeSearch = json.RawMessage(`{"results":[],"truncated":false,"mode":"lexical"}`)

func SearchToolDefinition() tools.Definition {
	return tools.Definition{
		Name:      tools.KBSearchToolName,
		Version:   tools.Version,
		Schema:    []byte(`{"$schema":"https://json-schema.org/draft/2020-12/schema","$id":"aura://tools/kb_search/v1","type":"object","properties":{"query":{"type":"string","minLength":1,"maxLength":500},"limit":{"type":"integer","minimum":1,"maximum":10}},"required":["query"],"additionalProperties":false}`),
		Validator: validateSearchArguments,
	}
}

func SearchToolRule() approval.Rule {
	return approval.Rule{
		ToolName:    tools.KBSearchToolName,
		ToolVersion: tools.Version,
		AllowedTrust: []approval.TrustLabel{
			approval.TrustOwnerInput,
			approval.TrustTrustedConfiguration,
			approval.TrustDerivedUntrusted,
		},
		Constraints: approval.Constraints{
			MaxOutputBytes: 64 * 1024,
			Timeout:        30 * time.Second,
		},
	}
}

func SearchToolRegistration(retriever *Retriever) toolbroker.ToolRegistration {
	return toolbroker.ToolRegistration{
		Definition: SearchToolDefinition(),
		Adapter:    NewToolAdapter(retriever),
		Rule:       SearchToolRule(),
	}
}

type toolProvider struct {
	retriever *Retriever
}

func NewToolProvider(retriever *Retriever) toolbroker.ToolProvider {
	return &toolProvider{retriever: retriever}
}

func (p *toolProvider) Tools() []toolbroker.ToolRegistration {
	return []toolbroker.ToolRegistration{SearchToolRegistration(p.retriever)}
}

func NewToolAdapter(retriever *Retriever) toolbroker.Adapter {
	return func(ctx context.Context, request *toolbroker.ToolRequest, _ approval.Constraints) (toolbroker.ToolResult, error) {
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
		if retriever == nil {
			return toolbroker.ToolResult{}, toolbroker.Errorf(toolbroker.ResultCapabilityUnavailable, "knowledge search is not available")
		}
		limit := DefaultSearchLimit
		if args.Limit != nil {
			limit = min(max(*args.Limit, 1), tools.MaxKBSearchResultLimit)
		}
		budgeted, cancel := context.WithTimeout(ctx, RetrievalBudget)
		defer cancel()
		found, err := retriever.Search(budgeted, &SearchRequest{
			Scope: request.Knowledge,
			Query: args.Query,
			Depth: SearchDepth,
			Class: request.PrincipalClass,
		})
		if err != nil {
			return toolbroker.ToolResult{}, toolbroker.Errorf(toolbroker.ResultCapabilityUnavailable, "knowledge search failed")
		}
		output := ShapeSearch(found.Hits, limit)
		output.Mode = found.Mode
		encoded, err := json.Marshal(output)
		if err != nil {
			return toolbroker.ToolResult{}, toolbroker.Errorf(toolbroker.ResultCapabilityUnavailable, "knowledge search failed")
		}
		return toolbroker.ToolResult{Class: toolbroker.ResultOK, Output: encoded}, nil
	}
}

func validateSearchArguments(raw json.RawMessage) (json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("arguments must be valid JSON: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("arguments must contain one JSON value")
		}
		return nil, fmt.Errorf("arguments have trailing data: %w", err)
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, errors.New("arguments must be a JSON object")
	}
	allowed := []string{"query", "limit"}
	for key := range object {
		if !slices.Contains(allowed, key) {
			return nil, fmt.Errorf("unknown argument %q", key)
		}
	}
	rawQuery, ok := object["query"]
	if !ok {
		return nil, fmt.Errorf("argument %q is required", "query")
	}
	query, ok := rawQuery.(string)
	if !ok || query == "" {
		return nil, fmt.Errorf("argument %q must be a non-empty string", "query")
	}
	if utf8.RuneCountInString(query) > tools.MaxKBSearchQueryRunes {
		return nil, fmt.Errorf("argument %q must be at most %d characters", "query", tools.MaxKBSearchQueryRunes)
	}
	if rawLimit, ok := object["limit"]; ok {
		number, ok := rawLimit.(json.Number)
		if !ok {
			return nil, fmt.Errorf("argument %q must be an integer", "limit")
		}
		n, err := number.Int64()
		if err != nil || n < 1 {
			return nil, fmt.Errorf("argument %q must be at least %d", "limit", 1)
		}
		if n > tools.MaxKBSearchResultLimit {
			return nil, fmt.Errorf("argument %q must be at most %d", "limit", tools.MaxKBSearchResultLimit)
		}
	}
	return json.Marshal(object)
}
