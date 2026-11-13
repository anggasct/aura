package runtimeadk

import (
	"encoding/json"
	"testing"

	auraagent "github.com/anggasct/aura/internal/platform/agent"
	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/turn"
	runtimeingress "github.com/anggasct/aura/internal/platform/turn/ingress"
)

func TestBuiltinToolRequestCarriesTheTaintOfUntrustedInput(t *testing.T) {
	history := &turn.UntrustedRecall{Trust: approval.TrustUntrustedExternal}
	cases := map[string]struct {
		origin  turn.Origin
		class   principal.Class
		context *turn.UntrustedRecall
		recall  RecallProvider
		want    bool
	}{
		"webhook":                  {origin: turn.OriginWebhook, class: principal.ClassTrusted, want: true},
		"http with client history": {origin: turn.OriginHTTP, class: principal.ClassTrusted, context: history, want: true},
		"child":                    {origin: turn.OriginChild, class: principal.ClassOwner, want: true},
		"http without history":     {origin: turn.OriginHTTP, class: principal.ClassTrusted},
		"http with recalled evidence": {
			origin: turn.OriginHTTP, class: principal.ClassTrusted, recall: &stubRecallProvider{evidence: corpusEvidence()},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
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
			definition := auraagent.Definition{ID: "team", PrincipalTools: []string{"sample_tool"}}
			options := []ExecutorOption{WithBuiltinToolExecutor(builtin), WithAgentResolver(&stubResolver{definition: definition}, nil)}
			if tc.recall != nil {
				options = append(options, WithRecallProvider(tc.recall))
			}
			executor, err := NewADKExecutor("aura", modelName, models, sessions, &fakeBroker{}, nil, nil, options...)
			if err != nil {
				t.Fatalf("NewADKExecutor: %v", err)
			}
			mustCreateSession(t, db, "session-1")
			req := &turn.TurnRequest{
				TurnID: "turn-1", SessionID: "session-1", PrincipalID: "user-1", PrincipalClass: tc.class,
				Origin: tc.origin, Parts: []runtimeingress.InputPart{{Text: "hi"}}, UntrustedContext: tc.context,
			}
			for _, err := range executor.Execute(t.Context(), req) {
				if err != nil {
					t.Fatalf("Execute: %v", err)
				}
			}
			if len(builtin.requests) != 1 || builtin.requests[0].Tainted != tc.want || len(builtin.requests[0].Knowledge) != 0 {
				t.Fatalf("builtin requests = %+v, want Tainted %v and no knowledge", builtin.requests, tc.want)
			}
		})
	}
}
