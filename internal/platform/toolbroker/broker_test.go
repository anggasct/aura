package toolbroker

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/capability/capabilitytest"
	"github.com/anggasct/aura/internal/platform/tools"
)

func brokerRequest(name, arguments string, capabilities ...string) *ToolRequest {
	return &ToolRequest{
		RequestID:      "request-1",
		TurnID:         "turn-1",
		SessionID:      "session-1",
		PrincipalID:    "owner-1",
		ToolName:       name,
		ToolVersion:    "v1",
		Arguments:      json.RawMessage(arguments),
		Capabilities:   capabilities,
		Trust:          approval.TrustOwnerInput,
		IdempotencyKey: "idempotency-1",
	}
}

func TestBrokerExposesAllBuiltins(t *testing.T) {
	broker, err := New(&Options{HostCapabilities: capabilitytest.Host(t)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := len(broker.Definitions()); got != 16 {
		t.Fatalf("Definitions = %d, want 16", got)
	}
}

func TestBrokerRejectsUnknownArgumentsBeforeAdapter(t *testing.T) {
	calls := 0
	broker, err := New(&Options{HostCapabilities: capabilitytest.Host(t), Adapters: map[string]Adapter{
		"read_file@v1": func(context.Context, *ToolRequest, approval.Constraints) (ToolResult, error) {
			calls++
			return ToolResult{Output: json.RawMessage(`{"ok":true}`)}, nil
		},
	}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = broker.Execute(context.Background(), brokerRequest("read_file", `{"path":"/tmp/file","extra":true}`, "workspace-read"))
	if class, ok := CodeOf(err); !ok || class != ResultInvalidArgument {
		t.Fatalf("CodeOf(%v) = %q, %v; want invalid_argument", err, class, ok)
	}
	if calls != 0 {
		t.Fatalf("adapter calls = %d, want 0", calls)
	}
}

func TestBrokerReportsMissingExecCapability(t *testing.T) {
	broker, err := New(&Options{HostCapabilities: capabilitytest.Host(t), Adapters: map[string]Adapter{
		"exec@v1": func(context.Context, *ToolRequest, approval.Constraints) (ToolResult, error) {
			t.Fatal("exec adapter reached without exec-linux capability")
			return ToolResult{}, nil
		},
	}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = broker.Execute(context.Background(), brokerRequest("exec", `{"command":["true"]}`))
	if class := classOf(err); class != ResultCapabilityUnavailable {
		t.Fatalf("error class = %q, want capability_unavailable (%v)", class, err)
	}
}

func TestBrokerObserverReceivesMetadataOnly(t *testing.T) {
	var observations []Observation
	broker, err := New(&Options{
		HostCapabilities: capabilitytest.Host(t), Adapters: map[string]Adapter{
			"read_file@v1": func(context.Context, *ToolRequest, approval.Constraints) (ToolResult, error) {
				return ToolResult{Output: json.RawMessage(`{"content":"untrusted"}`)}, nil
			},
		},
		Observer: func(_ context.Context, observation Observation) { observations = append(observations, observation) },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	request := brokerRequest("read_file", `{"path":"note.txt"}`, "workspace-read")
	if _, err := broker.Execute(context.Background(), request); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(observations) != 1 || observations[0].Class != ResultOK || observations[0].ToolName != "read_file" || observations[0].OutputBytes == 0 {
		t.Fatalf("observations = %+v", observations)
	}
}

func TestBrokerBindsExactApprovalAndReturnsUntrustedResult(t *testing.T) {
	broker, err := New(&Options{
		HostCapabilities: capabilitytest.Host(t), Effects: newBrokerEffectExecutor(t),
		Adapters: map[string]Adapter{
			"exec@v1": func(_ context.Context, request *ToolRequest, _ approval.Constraints) (ToolResult, error) {
				return ToolResult{Output: json.RawMessage(`{"command":"secret-value"}`)}, nil
			},
		},
		Secrets:              []string{"secret-value"},
		MaxInlineResultBytes: 1024,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	request := brokerRequest("exec", `{"command":["printf","ok"]}`, "exec-linux")
	request.EventSequence = 1
	if _, err := broker.Execute(context.Background(), request); classOf(err) != ResultApprovalRequired {
		t.Fatalf("Execute without approval = %v", err)
	}
	grant, err := broker.Grant(context.Background(), request, time.Minute)
	if err != nil {
		t.Fatalf("Grant: %v", err)
	}
	request.Approval = &grant
	result, err := broker.Execute(context.Background(), request)
	if err != nil {
		t.Fatalf("Execute with approval: %v", err)
	}
	if result.Class != ResultOK || !result.Untrusted || result.ToolName != "exec" || result.ToolVersion != "v1" {
		t.Fatalf("result = %+v", result)
	}
	if string(result.Output) != `{"command":"[REDACTED]"}` {
		t.Fatalf("redacted output = %s", result.Output)
	}
	request.Arguments = json.RawMessage(`{"command":["printf","changed"]}`)
	request.IdempotencyKey = "idempotency-changed"
	if _, err := broker.Execute(context.Background(), request); classOf(err) != ResultPolicyDenied {
		t.Fatalf("mutated approval error = %v", err)
	}
}

func classOf(err error) ResultClass {
	class, _ := CodeOf(err)
	return class
}

func TestBrokerRegisterAndUnregisterTool(t *testing.T) {
	broker, err := New(&Options{HostCapabilities: capabilitytest.Host(t)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	toolName := "custom_echo"
	toolVersion := "v1"

	def := tools.Definition{
		Name:    toolName,
		Version: toolVersion,
		Validator: func(raw json.RawMessage) (json.RawMessage, error) {
			return raw, nil
		},
	}
	adapter := func(_ context.Context, req *ToolRequest, _ approval.Constraints) (ToolResult, error) {
		return ToolResult{
			ToolName:    req.ToolName,
			ToolVersion: req.ToolVersion,
			Class:       ResultOK,
			Untrusted:   true,
			Output:      req.Arguments,
		}, nil
	}
	rule := approval.Rule{
		ToolName:     toolName,
		ToolVersion:  toolVersion,
		AllowedTrust: []approval.TrustLabel{approval.TrustOwnerInput},
		Constraints: approval.Constraints{
			MaxOutputBytes: 1024,
			Timeout:        10 * time.Second,
		},
	}

	if err := broker.RegisterTool(&def, adapter, &rule); err != nil {
		t.Fatalf("RegisterTool failed: %v", err)
	}

	req := brokerRequest(toolName, `{"msg":"hello"}`, "public-web")
	res, err := broker.Execute(t.Context(), req)
	if err != nil {
		t.Fatalf("Execute registered tool failed: %v", err)
	}
	if string(res.Output) != `{"msg":"hello"}` {
		t.Fatalf("output = %s, want %s", res.Output, `{"msg":"hello"}`)
	}

	broker.UnregisterTool(toolName, toolVersion)
	_, err = broker.Execute(t.Context(), req)
	if err == nil {
		t.Fatal("expected execute to fail after unregistering tool")
	}
}

func TestBrokerRunsTheToolArgumentCheckBeforeAnyEffect(t *testing.T) {
	cases := []struct {
		name      string
		check     ArgumentCheck
		wantClass ResultClass
		wantCalls int
	}{
		{"check passes", func(json.RawMessage) error { return nil }, ResultOK, 1},
		{"check rejects", func(json.RawMessage) error { return errors.New("source is too long") }, ResultInvalidArgument, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls, checked := 0, 0
			broker, err := New(&Options{
				HostCapabilities: capabilitytest.Host(t),
				Adapters: map[string]Adapter{"read_file@v1": func(context.Context, *ToolRequest, approval.Constraints) (ToolResult, error) {
					calls++
					return ToolResult{Output: json.RawMessage(`{"ok":true}`)}, nil
				}},
				ArgumentChecks: map[string]ArgumentCheck{"read_file@v1": func(arguments json.RawMessage) error {
					checked++
					if !json.Valid(arguments) {
						t.Errorf("the check received malformed arguments: %s", arguments)
					}
					return tc.check(arguments)
				}},
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			_, err = broker.Execute(context.Background(), brokerRequest("read_file", `{"path":"note.txt"}`, "workspace-read"))
			if class := classOf(err); tc.wantClass != ResultOK && class != tc.wantClass {
				t.Fatalf("error class = %q, want %q (%v)", class, tc.wantClass, err)
			}
			if tc.wantClass == ResultOK && err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if checked != 1 || calls != tc.wantCalls {
				t.Fatalf("checks = %d, adapter calls = %d; want 1 and %d", checked, calls, tc.wantCalls)
			}
		})
	}
}

func TestBrokerSkipsTheArgumentCheckOfOtherTools(t *testing.T) {
	checked := 0
	broker, err := New(&Options{
		HostCapabilities: capabilitytest.Host(t),
		Adapters: map[string]Adapter{"read_file@v1": func(context.Context, *ToolRequest, approval.Constraints) (ToolResult, error) {
			return ToolResult{Output: json.RawMessage(`{"ok":true}`)}, nil
		}},
		ArgumentChecks: map[string]ArgumentCheck{"exec@v1": func(json.RawMessage) error { checked++; return errors.New("never") }},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := broker.Execute(context.Background(), brokerRequest("read_file", `{"path":"note.txt"}`, "workspace-read")); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if checked != 0 {
		t.Fatalf("a check for another tool ran %d times", checked)
	}
}
