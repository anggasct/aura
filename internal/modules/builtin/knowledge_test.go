package toolsbuiltin

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"

	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/toolbroker"
)

type recordingSearcher struct {
	scope  []string
	query  string
	limit  int
	class  principal.Class
	calls  int
	output json.RawMessage
	err    error
}

func (s *recordingSearcher) SearchKnowledge(_ context.Context, scope []string, query string, limit int, class principal.Class) (json.RawMessage, error) {
	s.calls++
	s.scope, s.query, s.limit, s.class = slices.Clone(scope), query, limit, class
	return s.output, s.err
}

func knowledgeToolRequest(arguments string, knowledge ...string) *toolbroker.ToolRequest {
	return &toolbroker.ToolRequest{ToolName: "kb_search", ToolVersion: "v1", Arguments: json.RawMessage(arguments), Knowledge: knowledge}
}

func TestKnowledgeAdapterSearchesOnlyTheScopeFromTheTurn(t *testing.T) {
	searcher := &recordingSearcher{output: json.RawMessage(`{"results":[],"truncated":false}`)}
	holder := &knowledgeHolder{}
	holder.searcher.Store(func() *KnowledgeSearcher { var s KnowledgeSearcher = searcher; return &s }())

	result, err := holder.adapter(t.Context(), knowledgeToolRequest(`{"query":"refund policy"}`, "faq", "support"), approval.Constraints{})
	if err != nil || result.Class != toolbroker.ResultOK || string(result.Output) != `{"results":[],"truncated":false}` {
		t.Fatalf("adapter = %+v, %v", result, err)
	}
	if !slices.Equal(searcher.scope, []string{"faq", "support"}) || searcher.query != "refund policy" || searcher.limit != 5 {
		t.Errorf("searched scope=%v query=%q limit=%d, want the turn scope and the default limit 5", searcher.scope, searcher.query, searcher.limit)
	}
}

func TestKnowledgeAdapterClampsTheLimit(t *testing.T) {
	for name, test := range map[string]struct {
		arguments string
		want      int
	}{
		"explicit": {`{"query":"x","limit":3}`, 3},
		"maximum":  {`{"query":"x","limit":10}`, 10},
		"over":     {`{"query":"x","limit":99}`, 10},
		"zero":     {`{"query":"x","limit":0}`, 1},
	} {
		t.Run(name, func(t *testing.T) {
			searcher := &recordingSearcher{output: json.RawMessage(`{}`)}
			holder := &knowledgeHolder{}
			holder.searcher.Store(func() *KnowledgeSearcher { var s KnowledgeSearcher = searcher; return &s }())
			if _, err := holder.adapter(t.Context(), knowledgeToolRequest(test.arguments, "faq"), approval.Constraints{}); err != nil {
				t.Fatalf("adapter: %v", err)
			}
			if searcher.limit != test.want {
				t.Errorf("limit = %d, want %d", searcher.limit, test.want)
			}
		})
	}
}

func TestKnowledgeAdapterReturnsNothingForAnEmptyScope(t *testing.T) {
	searcher := &recordingSearcher{output: json.RawMessage(`{"results":[{"id":"leak"}],"truncated":false}`)}
	holder := &knowledgeHolder{}
	holder.searcher.Store(func() *KnowledgeSearcher { var s KnowledgeSearcher = searcher; return &s }())
	result, err := holder.adapter(t.Context(), knowledgeToolRequest(`{"query":"refund"}`), approval.Constraints{})
	if err != nil || string(result.Output) != `{"results":[],"truncated":false,"mode":"lexical"}` || searcher.calls != 0 {
		t.Fatalf("result = %s, %v, searcher calls = %d, want an empty answer without searching", result.Output, err, searcher.calls)
	}
}

func TestKnowledgeAdapterFailsClosed(t *testing.T) {
	unavailable := &knowledgeHolder{}
	if _, err := unavailable.adapter(t.Context(), knowledgeToolRequest(`{"query":"x"}`, "faq"), approval.Constraints{}); !isClass(err, toolbroker.ResultCapabilityUnavailable) {
		t.Errorf("no searcher: %v", err)
	}
	failing := &knowledgeHolder{}
	failing.searcher.Store(func() *KnowledgeSearcher {
		var s KnowledgeSearcher = &recordingSearcher{err: errors.New("secret detail")}
		return &s
	}())
	_, err := failing.adapter(t.Context(), knowledgeToolRequest(`{"query":"x"}`, "faq"), approval.Constraints{})
	if !isClass(err, toolbroker.ResultCapabilityUnavailable) || err.Error() == "" || errors.Unwrap(err) != nil {
		t.Errorf("failing searcher: %v", err)
	}
	if _, err := failing.adapter(t.Context(), knowledgeToolRequest(`{"query":""}`, "faq"), approval.Constraints{}); !isClass(err, toolbroker.ResultInvalidArgument) {
		t.Errorf("empty query: %v", err)
	}
	if _, err := failing.adapter(t.Context(), nil, approval.Constraints{}); !isClass(err, toolbroker.ResultInvalidArgument) {
		t.Errorf("nil request: %v", err)
	}
}

func TestSetKnowledgeSearcherIgnoresNil(t *testing.T) {
	var executor *Executor
	executor.SetKnowledgeSearcher(&recordingSearcher{})
	holder := &knowledgeHolder{}
	(&Executor{knowledge: holder}).SetKnowledgeSearcher(nil)
	if holder.searcher.Load() != nil {
		t.Error("nil searcher stored")
	}
}

func isClass(err error, class toolbroker.ResultClass) bool {
	got, ok := toolbroker.CodeOf(err)
	return ok && got == class
}

func TestKnowledgeAdapterPassesTheRequestClassWithoutDefaultingIt(t *testing.T) {
	tests := map[string]principal.Class{
		"owner":   principal.ClassOwner,
		"trusted": principal.ClassTrusted,
		"public":  principal.ClassPublic,
		"empty":   "",
	}
	for name, class := range tests {
		t.Run(name, func(t *testing.T) {
			searcher := &recordingSearcher{output: json.RawMessage(`{}`)}
			holder := &knowledgeHolder{}
			holder.searcher.Store(func() *KnowledgeSearcher { var s KnowledgeSearcher = searcher; return &s }())
			request := knowledgeToolRequest(`{"query":"refund"}`, "faq")
			request.PrincipalClass = class
			if _, err := holder.adapter(t.Context(), request, approval.Constraints{}); err != nil {
				t.Fatalf("adapter: %v", err)
			}
			if searcher.class != class {
				t.Errorf("searcher saw class %q, want %q", searcher.class, class)
			}
		})
	}
}
