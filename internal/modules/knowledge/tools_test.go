package knowledge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/toolbroker"
	"github.com/anggasct/aura/internal/platform/tools"
)

func knowledgeToolRequest(arguments string, knowledge ...string) *toolbroker.ToolRequest {
	return &toolbroker.ToolRequest{
		ToolName:    tools.KBSearchToolName,
		ToolVersion: tools.Version,
		Arguments:   json.RawMessage(arguments),
		Knowledge:   knowledge,
	}
}

func newTestToolRetriever(t *testing.T, index Index, opts ...RetrieverOption) *Retriever {
	t.Helper()
	retriever, err := NewRetriever(index, &fakeSecrets{}, opts...)
	if err != nil {
		t.Fatalf("NewRetriever: %v", err)
	}
	return retriever
}

func TestSearchToolDefinitionMatchesBuiltinCatalog(t *testing.T) {
	got := SearchToolDefinition()
	if got.Name != tools.KBSearchToolName || got.Version != tools.Version || got.Validator == nil {
		t.Fatalf("SearchToolDefinition() = %+v", got)
	}
	if got.OptIn || got.Effectful || got.RequiresApproval {
		t.Fatalf("kb_search definition = %+v, want a plain read-tier tool", got)
	}

	payloads := []struct {
		name string
		raw  string
		ok   bool
	}{
		{name: "minimal", raw: `{"query":"refund policy"}`, ok: true},
		{name: "with limit min", raw: `{"query":"refund policy","limit":1}`, ok: true},
		{name: "with limit max", raw: `{"query":"refund policy","limit":10}`, ok: true},
		{name: "max runes", raw: fmt.Sprintf(`{"query":%q}`, strings.Repeat("a", MaxSearchQueryRunes)), ok: true},
		{name: "over max runes", raw: fmt.Sprintf(`{"query":%q}`, strings.Repeat("a", MaxSearchQueryRunes+1)), ok: false},
		{name: "empty query", raw: `{"query":""}`, ok: false},
		{name: "missing query", raw: `{"limit":3}`, ok: false},
		{name: "non-string query", raw: `{"query":123}`, ok: false},
		{name: "limit zero", raw: `{"query":"x","limit":0}`, ok: false},
		{name: "limit negative", raw: `{"query":"x","limit":-1}`, ok: false},
		{name: "limit over max", raw: `{"query":"x","limit":11}`, ok: false},
		{name: "limit float", raw: `{"query":"x","limit":1.5}`, ok: false},
		{name: "limit string", raw: `{"query":"x","limit":"5"}`, ok: false},
		{name: "unknown key", raw: `{"query":"x","collection":"secret"}`, ok: false},
		{name: "ambiguous key", raw: `{"query":"a","query":"b"}`, ok: false},
		{name: "trailing data", raw: `{"query":"a"} {}`, ok: false},
		{name: "not object", raw: `["a"]`, ok: false},
		{name: "invalid json", raw: `{`, ok: false},
	}
	for _, tc := range payloads {
		t.Run(tc.name, func(t *testing.T) {
			gotOut, gotErr := got.Validate(json.RawMessage(tc.raw))
			if (gotErr == nil) != tc.ok {
				t.Fatalf("got.Validate(%s) err = %v, want ok=%t", tc.raw, gotErr, tc.ok)
			}
			if tc.ok && !json.Valid(gotOut) {
				t.Fatalf("validated output is not valid JSON: %s", gotOut)
			}
		})
	}
}

func TestSearchToolRuleMatchesDefaultPolicy(t *testing.T) {
	def := SearchToolDefinition()
	want := toolbroker.DefaultRule(&def)
	got := SearchToolRule()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("SearchToolRule() = %+v, want %+v", got, want)
	}
}

func TestKnowledgeAdapterSearchesOnlyTheScopeFromTheTurn(t *testing.T) {
	index := &contextTestIndex{hits: []Hit{
		{ChunkID: "kbc_1", Collection: "faq", Label: "faq.md", Heading: "Refunds", Ordinal: 1, Content: "Refunds are issued within thirty days."},
	}}
	adapter := NewToolAdapter(newTestToolRetriever(t, index))

	result, err := adapter(t.Context(), knowledgeToolRequest(`{"query":"refund policy"}`, "faq", "support"), approval.Constraints{})
	if err != nil || result.Class != toolbroker.ResultOK {
		t.Fatalf("adapter = %+v, %v", result, err)
	}
	var out SearchOutput
	if err := json.Unmarshal(result.Output, &out); err != nil {
		t.Fatalf("decode %s: %v", result.Output, err)
	}
	if len(out.Results) != 1 || out.Results[0].ID != "kbc_1" || out.Mode != ModeLexical {
		t.Errorf("output = %+v", out)
	}
	if index.calls != 1 || !slices.Equal(index.scopes[0], []string{"faq", "support"}) {
		t.Errorf("searched scope=%v calls=%d, want [faq support]", index.scopes, index.calls)
	}
}

func TestKnowledgeAdapterClampsTheLimit(t *testing.T) {
	var hits []Hit
	for i := range 12 {
		hits = append(hits, Hit{
			ChunkID:    fmt.Sprintf("kbc_%d", i),
			Collection: "faq",
			Label:      "faq.md",
			Heading:    "Refunds",
			Ordinal:    i,
			Content:    "refund policy",
		})
	}
	for name, test := range map[string]struct {
		arguments string
		want      int
	}{
		"explicit": {`{"query":"refund","limit":3}`, 3},
		"maximum":  {`{"query":"refund","limit":10}`, 10},
		"over":     {`{"query":"refund","limit":99}`, 10},
		"zero":     {`{"query":"refund","limit":0}`, 1},
	} {
		t.Run(name, func(t *testing.T) {
			index := &contextTestIndex{hits: hits}
			adapter := NewToolAdapter(newTestToolRetriever(t, index))
			result, err := adapter(t.Context(), knowledgeToolRequest(test.arguments, "faq"), approval.Constraints{})
			if err != nil {
				t.Fatalf("adapter: %v", err)
			}
			var out SearchOutput
			if err := json.Unmarshal(result.Output, &out); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if len(out.Results) != test.want {
				t.Errorf("results = %d, want %d", len(out.Results), test.want)
			}
		})
	}
}

func TestKnowledgeAdapterReturnsNothingForAnEmptyScope(t *testing.T) {
	index := &contextTestIndex{hits: sampleContextHits()}
	adapter := NewToolAdapter(newTestToolRetriever(t, index))
	result, err := adapter(t.Context(), knowledgeToolRequest(`{"query":"refund"}`), approval.Constraints{})
	if err != nil || string(result.Output) != `{"results":[],"truncated":false,"mode":"lexical"}` || index.calls != 0 {
		t.Fatalf("result = %s, %v, calls = %d, want an empty answer without searching", result.Output, err, index.calls)
	}
}

func TestKnowledgeAdapterFailsClosed(t *testing.T) {
	unavailable := NewToolAdapter(nil)
	if _, err := unavailable(t.Context(), knowledgeToolRequest(`{"query":"x"}`, "faq"), approval.Constraints{}); !fault.HasCode(err, toolbroker.ResultCapabilityUnavailable) {
		t.Errorf("no retriever: %v", err)
	}
	failing := NewToolAdapter(newTestToolRetriever(t, &contextTestIndex{err: errors.New("secret detail")}))
	_, err := failing(t.Context(), knowledgeToolRequest(`{"query":"x"}`, "faq"), approval.Constraints{})
	if !fault.HasCode(err, toolbroker.ResultCapabilityUnavailable) || err.Error() == "" || errors.Unwrap(err) != nil {
		t.Errorf("failing retriever: %v", err)
	}
	if _, err := failing(t.Context(), knowledgeToolRequest(`{"query":""}`, "faq"), approval.Constraints{}); !fault.HasCode(err, toolbroker.ResultInvalidArgument) {
		t.Errorf("empty query: %v", err)
	}
	if _, err := failing(t.Context(), nil, approval.Constraints{}); !fault.HasCode(err, toolbroker.ResultInvalidArgument) {
		t.Errorf("nil request: %v", err)
	}
}

func TestKnowledgeAdapterPassesTheRequestClassWithoutDefaultingIt(t *testing.T) {
	settings := &EmbeddingSettings{Fingerprint: "fp", Destination: "api.example.com", Model: "m", Dimensions: 4, BatchSize: 96, MaxMemory: 1 << 20}
	tests := map[string]struct {
		class     principal.Class
		wantSends int
		wantMode  string
	}{
		"owner":   {class: principal.ClassOwner, wantSends: 1, wantMode: ModeHybrid},
		"trusted": {class: principal.ClassTrusted, wantSends: 1, wantMode: ModeHybrid},
		"public":  {class: principal.ClassPublic, wantSends: 0, wantMode: ModeLexical},
		"empty":   {class: "", wantSends: 0, wantMode: ModeLexical},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			embedder := &countingEmbedder{vector: []float32{1, 0, 0, 0}}
			view := &fakeView{size: 1, hits: []Hit{{ChunkID: "kbc_1", Collection: "faq", Label: "faq.md", Ordinal: 1, Content: "refund"}}}
			retriever := newTestToolRetriever(t, &contextTestIndex{hits: view.hits}, WithVectors(&VectorConfig{
				Index:    &fakeVectorIndex{view: view},
				Embedder: embedder,
				Settings: settings,
			}))
			adapter := NewToolAdapter(retriever)
			request := knowledgeToolRequest(`{"query":"refund"}`, "faq")
			request.PrincipalClass = tc.class
			result, err := adapter(context.Background(), request, approval.Constraints{})
			if err != nil {
				t.Fatalf("adapter: %v", err)
			}
			var out SearchOutput
			if err := json.Unmarshal(result.Output, &out); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if out.Mode != tc.wantMode || len(embedder.calls) != tc.wantSends {
				t.Errorf("mode = %q sends = %d, want %q and %d", out.Mode, len(embedder.calls), tc.wantMode, tc.wantSends)
			}
		})
	}
}
