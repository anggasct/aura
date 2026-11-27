package runtimeadk

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/genai"

	"github.com/anggasct/aura/internal/platform/approval"
	auramodel "github.com/anggasct/aura/internal/platform/model"
	"github.com/anggasct/aura/internal/platform/toolbroker"
	"github.com/anggasct/aura/internal/platform/tools"
)

type argumentRecorder struct {
	arguments []string
}

func (r *argumentRecorder) register(t *testing.T, broker *toolbroker.Broker, name string, validate func(map[string]any) error) {
	t.Helper()
	definition := &tools.Definition{
		Name: name, Version: "v1", Schema: json.RawMessage(`{"type":"object"}`),
		Validator: func(raw json.RawMessage) (json.RawMessage, error) {
			object, err := tools.DecodeObject(raw)
			if err != nil {
				return nil, err
			}
			if err := validate(object); err != nil {
				return nil, err
			}
			return raw, nil
		},
	}
	rule := toolbroker.DefaultRule(definition)
	if err := broker.RegisterTool(definition, func(_ context.Context, req *toolbroker.ToolRequest, _ approval.Constraints) (toolbroker.ToolResult, error) {
		r.arguments = append(r.arguments, string(req.Arguments))
		return toolbroker.ToolResult{Output: json.RawMessage(`{"listed":true}`)}, nil
	}, &rule); err != nil {
		t.Fatalf("RegisterTool %s: %v", name, err)
	}
}

func argumentlessCall(name string) *genai.Part {
	return &genai.Part{FunctionCall: &genai.FunctionCall{ID: "call_1", Name: name}}
}

func TestArgumentlessCallReachesTheToolAsAnEmptyObject(t *testing.T) {
	cases := []struct {
		name          string
		tool          string
		wantArguments []string
		wantResponse  string
	}{
		{name: "a tool without required arguments runs", tool: "list_items", wantArguments: []string{`{}`}, wantResponse: `"listed":true`},
		{name: "a tool with a required argument names it", tool: "run_command", wantResponse: `argument \"command\" is required`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			broker, err := toolbroker.New(&toolbroker.Options{})
			if err != nil {
				t.Fatalf("toolbroker.New: %v", err)
			}
			recorder := &argumentRecorder{}
			recorder.register(t, broker, "list_items", func(object map[string]any) error { return tools.RejectUnknown(object) })
			recorder.register(t, broker, "run_command", func(object map[string]any) error { return tools.ValidateStringArray(object, "command", true) })
			builtins, err := NewBrokerExecutor(broker, nil)
			if err != nil {
				t.Fatalf("NewBrokerExecutor: %v", err)
			}
			model := &scriptedModel{responses: [][]*genai.Part{{argumentlessCall(tc.tool)}, textResponse()}}
			executor := memoryExecutor(t, model, builtins, "session-1")

			if err := runTurn(t.Context(), executor, sessionTurn("session-1")); err != nil {
				t.Fatalf("Execute: %v", err)
			}

			if strings.Join(recorder.arguments, ",") != strings.Join(tc.wantArguments, ",") {
				t.Errorf("tool arguments = %q, want %q", recorder.arguments, tc.wantArguments)
			}
			requests := model.seen()
			if len(requests) != 2 {
				t.Fatalf("model calls = %d, want 2", len(requests))
			}
			outputs := responseOutputs(requests[1])
			if len(outputs) != 1 || !strings.Contains(outputs[0], tc.wantResponse) {
				t.Errorf("function responses = %q, want one containing %s", outputs, tc.wantResponse)
			}
			if len(outputs) == 1 && strings.Contains(outputs[0], "must be a JSON object") {
				t.Errorf("function response = %s, want no object refusal", outputs[0])
			}
		})
	}
}

type gateContext struct {
	agent.Context
}

func (gateContext) InvocationID() string { return "invocation-1" }
func (gateContext) SessionID() string    { return "session-1" }
func (gateContext) UserID() string       { return "user-1" }

func TestToolGateMarshalsAnEmptyObject(t *testing.T) {
	broker := &fakeBroker{}
	model := &scriptedModel{}
	executor, _, _ := newADKTestExecutor(t, model.Name(), auramodel.Set{model.Name(): model}, broker)

	if err := executor.toolGate(gateContext{}, "list_items", nil); err != nil {
		t.Fatalf("toolGate: %v", err)
	}

	if len(broker.requests) != 1 || string(broker.requests[0].Arguments) != `{}` {
		t.Fatalf("broker requests = %+v, want one with arguments {}", broker.requests)
	}
}

func TestNonObjectArgumentsAreStillRefused(t *testing.T) {
	for _, raw := range []string{`[]`, `"abc"`, `1`, `null`} {
		t.Run(raw, func(t *testing.T) {
			if _, err := tools.DecodeObject(json.RawMessage(raw)); err == nil {
				t.Fatalf("DecodeObject(%s) = nil error, want a refusal", raw)
			}
		})
	}
}
