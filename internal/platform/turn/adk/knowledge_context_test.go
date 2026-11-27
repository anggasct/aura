package runtimeadk

import (
	"encoding/json"
	"slices"
	"testing"

	auraagent "github.com/anggasct/aura/internal/platform/agent"
	"github.com/anggasct/aura/internal/platform/principal"
	auratools "github.com/anggasct/aura/internal/platform/tools"
	"github.com/anggasct/aura/internal/platform/turn"
	runtimeingress "github.com/anggasct/aura/internal/platform/turn/ingress"

	"google.golang.org/adk/v2/tool"
)

func TestToolsForOffersKnowledgeSearchOnlyWithAStampedList(t *testing.T) {
	executor := &ADKExecutor{
		tools: []tool.Tool{namedToolStub{name: "read_file"}, namedToolStub{name: auratools.KBSearchToolName}},
	}
	names := func(tools []tool.Tool) []string {
		out := make([]string, 0, len(tools))
		for _, t := range tools {
			out = append(out, t.Name())
		}
		return out
	}
	listed := &auraagent.Definition{
		Audience: "public", Tools: []string{"read_file", auratools.KBSearchToolName},
		PrincipalTools: []string{"read_file", auratools.KBSearchToolName}, PublicTools: []string{auratools.KBSearchToolName},
	}
	explicitWithoutSearch := &auraagent.Definition{Tools: []string{"read_file"}, PrincipalTools: []string{"read_file"}, PublicTools: []string{}}
	scope := []string{"support"}
	cases := map[string]struct {
		definition *auraagent.Definition
		class      principal.Class
		knowledge  []string
		want       []string
	}{
		"owner with knowledge, implicit tools":     {&auraagent.Definition{}, principal.ClassOwner, scope, []string{"read_file", "kb_search"}},
		"owner without knowledge":                  {&auraagent.Definition{}, principal.ClassOwner, nil, []string{"read_file"}},
		"owner explicit list bypassed":             {explicitWithoutSearch, principal.ClassOwner, scope, []string{"read_file", "kb_search"}},
		"owner explicit list without knowledge":    {explicitWithoutSearch, principal.ClassOwner, nil, []string{"read_file"}},
		"trusted listed":                           {listed, principal.ClassTrusted, scope, []string{"read_file", "kb_search"}},
		"trusted listed but no knowledge":          {listed, principal.ClassTrusted, nil, []string{"read_file"}},
		"trusted not listed":                       {explicitWithoutSearch, principal.ClassTrusted, scope, []string{"read_file"}},
		"public listed":                            {listed, principal.ClassPublic, scope, []string{"kb_search"}},
		"public not listed":                        {explicitWithoutSearch, principal.ClassPublic, scope, nil},
		"unclassified without knowledge":           {listed, "", nil, []string{"read_file"}},
		"persona without knowledge, implicit list": {&auraagent.Definition{}, principal.ClassOwner, nil, []string{"read_file"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := names(executor.toolsFor(tc.definition, tc.class, "", turnContext{knowledgeScope: tc.knowledge}))
			if len(got) != len(tc.want) || (len(got) > 0 && !slices.Equal(got, tc.want)) {
				t.Fatalf("tools = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestBuiltinToolRequestCarriesTheStampedKnowledgeList(t *testing.T) {
	model := &fakeADKModel{answer: "call tool", toolCall: true, tokens: 3}
	modelName, models := fakeModelSet(model)
	builtin := &fakeBuiltinExecutor{
		definitions: []BuiltinToolDefinition{{
			Name: "sample_tool", Version: "v1", Description: "sample",
			Schema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}},"required":["query"],"additionalProperties":false}`),
		}},
		output: json.RawMessage(`{"ok":true}`),
	}
	db, sessions, _ := newSessionTestDB(t)
	executor, err := NewADKExecutor("aura", modelName, models, sessions, &fakeBroker{}, nil, nil, WithBuiltinToolExecutor(builtin))
	if err != nil {
		t.Fatalf("NewADKExecutor: %v", err)
	}
	mustCreateSession(t, db, "session-1")
	req := &turn.TurnRequest{
		TurnID: "turn-1", SessionID: "session-1", PrincipalID: "user-1", PrincipalClass: principal.ClassOwner,
		Origin: turn.OriginTerminal, Parts: []runtimeingress.InputPart{{Text: "hi"}}, Knowledge: []string{"faq", "support"},
	}
	for _, err := range executor.Execute(t.Context(), req) {
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
	}
	if len(builtin.requests) != 1 || !slices.Equal(builtin.requests[0].Knowledge, []string{"faq", "support"}) || !builtin.requests[0].Tainted {
		t.Fatalf("builtin requests = %+v, want the stamped list and the taint flag on the tool call", builtin.requests)
	}
}

func TestBuiltinToolRequestIsUntaintedWithoutAStampedList(t *testing.T) {
	model := &fakeADKModel{answer: "call tool", toolCall: true, tokens: 3}
	modelName, models := fakeModelSet(model)
	builtin := &fakeBuiltinExecutor{
		definitions: []BuiltinToolDefinition{{
			Name: "sample_tool", Version: "v1", Description: "sample",
			Schema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}},"required":["query"],"additionalProperties":false}`),
		}},
		output: json.RawMessage(`{"ok":true}`),
	}
	db, sessions, _ := newSessionTestDB(t)
	executor, err := NewADKExecutor("aura", modelName, models, sessions, &fakeBroker{}, nil, nil, WithBuiltinToolExecutor(builtin))
	if err != nil {
		t.Fatalf("NewADKExecutor: %v", err)
	}
	mustCreateSession(t, db, "session-1")
	req := &turn.TurnRequest{
		TurnID: "turn-1", SessionID: "session-1", PrincipalID: "user-1", PrincipalClass: principal.ClassOwner,
		Origin: turn.OriginTerminal, Parts: []runtimeingress.InputPart{{Text: "hi"}},
	}
	for _, err := range executor.Execute(t.Context(), req) {
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
	}
	if len(builtin.requests) != 1 || builtin.requests[0].Tainted || len(builtin.requests[0].Knowledge) != 0 {
		t.Fatalf("builtin requests = %+v, want an untainted call", builtin.requests)
	}
}
