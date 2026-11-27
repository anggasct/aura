package runtimeadk

import (
	"context"
	"encoding/json"
	"testing"

	auraagent "github.com/anggasct/aura/internal/platform/agent"
	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/toolbroker"
	auratools "github.com/anggasct/aura/internal/platform/tools"
	"github.com/anggasct/aura/internal/platform/turn"
	runtimeingress "github.com/anggasct/aura/internal/platform/turn/ingress"
)

type readOperationProvider struct {
	registration toolbroker.ToolRegistration
}

func (p readOperationProvider) Tools() []toolbroker.ToolRegistration {
	return []toolbroker.ToolRegistration{p.registration}
}

func TestBuiltinToolRequestCarriesTheTurnOrigin(t *testing.T) {
	origins := []turn.Origin{turn.OriginTerminal, turn.OriginSchedule, turn.OriginHTTP, turn.OriginChild, turn.OriginWorkflow, "telegram:main"}
	for _, origin := range origins {
		t.Run(string(origin), func(t *testing.T) {
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
			executor, err := NewADKExecutor("aura", modelName, models, sessions, &fakeBroker{}, nil, nil, WithBuiltinToolExecutor(builtin), WithAgentResolver(&stubResolver{definition: definition}, nil))
			if err != nil {
				t.Fatalf("NewADKExecutor: %v", err)
			}
			mustCreateSession(t, db, "session-1")
			req := &turn.TurnRequest{
				TurnID: "turn-1", SessionID: "session-1", PrincipalID: "user-1", PrincipalClass: principal.ClassOwner,
				Origin: origin, Parts: []runtimeingress.InputPart{{Text: "hi"}},
			}
			for _, err := range executor.Execute(t.Context(), req) {
				if err != nil {
					t.Fatalf("Execute: %v", err)
				}
			}
			if len(builtin.requests) != 1 || builtin.requests[0].Origin != origin {
				t.Fatalf("builtin requests = %+v, want the origin %q", builtin.requests, origin)
			}
		})
	}
}

func TestBrokerExecutorForwardsTheOriginToTheOperationRules(t *testing.T) {
	definition := auratools.Definition{
		Name: "aura_probe_read", Version: auratools.Version, Risk: auratools.RiskRead, Operation: "probe.read",
		Validator: func(raw json.RawMessage) (json.RawMessage, error) { return raw, nil },
	}
	if err := definition.ApplyOperationFlags(); err != nil {
		t.Fatalf("ApplyOperationFlags: %v", err)
	}
	broker, err := toolbroker.New(&toolbroker.Options{})
	if err != nil {
		t.Fatalf("toolbroker.New: %v", err)
	}
	err = broker.RegisterProvider(readOperationProvider{registration: toolbroker.ToolRegistration{
		Definition: definition, Rule: toolbroker.DefaultRule(&definition),
		Adapter: func(context.Context, *toolbroker.ToolRequest, approval.Constraints) (toolbroker.ToolResult, error) {
			return toolbroker.ToolResult{Output: json.RawMessage(`{"ok":true}`)}, nil
		},
	}})
	if err != nil {
		t.Fatalf("RegisterProvider: %v", err)
	}
	executor, err := NewBrokerExecutor(broker, nil)
	if err != nil {
		t.Fatalf("NewBrokerExecutor: %v", err)
	}
	cases := []struct {
		name   string
		origin turn.Origin
		want   toolbroker.ResultClass
	}{
		{name: "a terminal turn", origin: turn.OriginTerminal},
		{name: "a scheduled turn", origin: turn.OriginSchedule},
		{name: "a child run", origin: turn.OriginChild, want: toolbroker.ResultToolTierDenied},
		{name: "a workflow run", origin: turn.OriginWorkflow, want: toolbroker.ResultToolTierDenied},
		{name: "a request that names no origin", want: toolbroker.ResultToolTierDenied},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := executor.Execute(t.Context(), &BuiltinToolRequest{
				RequestID: "request-1", TurnID: "turn-1", SessionID: "session-1", PrincipalID: "owner-1",
				PrincipalClass: principal.ClassOwner, ToolName: definition.Name, ToolVersion: definition.Version,
				Arguments: json.RawMessage(`{}`), Trust: string(approval.TrustOwnerInput), IdempotencyKey: "key-1", Origin: tc.origin,
			})
			if code, _ := fault.CodeOf[toolbroker.ResultClass](err); code != tc.want {
				t.Fatalf("Execute error = %v, want code %q", err, tc.want)
			}
		})
	}
}
