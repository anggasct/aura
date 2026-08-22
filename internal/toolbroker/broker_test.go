package toolbroker

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/approval"
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
	broker, err := New(Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := len(broker.Definitions()); got != 6 {
		t.Fatalf("Definitions = %d, want 6", got)
	}
}

func TestBrokerRejectsUnknownArgumentsBeforeAdapter(t *testing.T) {
	calls := 0
	broker, err := New(Options{Adapters: map[string]Adapter{
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
	broker, err := New(Options{Adapters: map[string]Adapter{
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
	broker, err := New(Options{
		Adapters: map[string]Adapter{
			"read_file@v1": func(context.Context, *ToolRequest, approval.Constraints) (ToolResult, error) {
				return ToolResult{Output: json.RawMessage(`{"content":"untrusted"}`)}, nil
			},
		},
		Observer: func(observation Observation) { observations = append(observations, observation) },
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
	broker, err := New(Options{
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
	if _, err := broker.Execute(context.Background(), request); classOf(err) != ResultPolicyDenied {
		t.Fatalf("mutated approval error = %v", err)
	}
}

func classOf(err error) ResultClass {
	class, _ := CodeOf(err)
	return class
}
