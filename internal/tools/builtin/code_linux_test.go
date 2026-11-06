//go:build linux

package toolsbuiltin

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/anggasct/aura/internal/approval"
	"github.com/anggasct/aura/internal/capability/capabilitytest"
	"github.com/anggasct/aura/internal/principal"
	runtimeadk "github.com/anggasct/aura/internal/runtime/adk"
	"github.com/anggasct/aura/internal/toolbroker"
)

type fakeCodeRunner struct {
	calls  int
	check  error
	output json.RawMessage
}

func (f *fakeCodeRunner) Adapter() toolbroker.Adapter {
	return func(context.Context, *toolbroker.ToolRequest, approval.Constraints) (toolbroker.ToolResult, error) {
		f.calls++
		return toolbroker.ToolResult{Class: toolbroker.ResultOK, Output: f.output}, nil
	}
}

func (f *fakeCodeRunner) ArgumentCheck(json.RawMessage) error {
	return f.check
}

const codeArguments = `{"runtime":"python","source":"print(1)"}`

func TestCodeExecuteWithoutARunnerIsUnavailableAfterApproval(t *testing.T) {
	fixture := newGateFixture(t, capabilitytest.Report(t, "code-interpreter"), true)
	_, err := fixture.call("code_execute", codeArguments, "code-interpreter")
	if code, ok := toolbroker.CodeOf(err); !ok || code != toolbroker.ResultCapabilityUnavailable {
		t.Fatalf("code_execute error = %v, want capability_unavailable", err)
	}
}

func TestCodeExecuteNeedsApprovalLikeExec(t *testing.T) {
	fixture := newGateFixture(t, capabilitytest.Report(t, "code-interpreter"), false)
	runner := &fakeCodeRunner{output: json.RawMessage(`{}`)}
	fixture.executor.SetCodeRunner(runner)
	_, err := fixture.call("code_execute", codeArguments, "code-interpreter")
	if code, ok := toolbroker.CodeOf(err); !ok || code != toolbroker.ResultApprovalRequired {
		t.Fatalf("code_execute error = %v, want approval_required", err)
	}
	if fixture.prompts != 1 || runner.calls != 0 {
		t.Fatalf("prompts = %d, runner calls = %d; want one prompt and no run", fixture.prompts, runner.calls)
	}
}

func TestCodeExecuteRunsThroughTheInstalledRunner(t *testing.T) {
	fixture := newGateFixture(t, capabilitytest.Report(t, "code-interpreter"), true)
	runner := &fakeCodeRunner{output: json.RawMessage(`{"result_class":"ok"}`)}
	fixture.executor.SetCodeRunner(runner)
	output, err := fixture.call("code_execute", codeArguments, "code-interpreter")
	if err != nil {
		t.Fatalf("code_execute: %v", err)
	}
	if runner.calls != 1 || string(output) != `{"result_class":"ok"}` {
		t.Fatalf("runner calls = %d, output = %s", runner.calls, output)
	}
}

func TestCodeExecuteRejectsBadArgumentsBeforeAnyPrompt(t *testing.T) {
	cases := []struct {
		name      string
		arguments string
		check     error
	}{
		{"unknown runtime", `{"runtime":"ruby","source":"x"}`, nil},
		{"unknown field", `{"runtime":"python","source":"x","env":{}}`, nil},
		{"missing source", `{"runtime":"python"}`, nil},
		{"configured limit", codeArguments, errors.New("argument \"source\" must be at most 4 bytes")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newGateFixture(t, capabilitytest.Report(t, "code-interpreter"), true)
			runner := &fakeCodeRunner{check: tc.check, output: json.RawMessage(`{}`)}
			fixture.executor.SetCodeRunner(runner)
			_, err := fixture.call("code_execute", tc.arguments, "code-interpreter")
			if code, ok := toolbroker.CodeOf(err); !ok || code != toolbroker.ResultInvalidArgument {
				t.Fatalf("code_execute error = %v, want invalid_argument", err)
			}
			if fixture.prompts != 0 || runner.calls != 0 {
				t.Fatalf("prompts = %d, runner calls = %d; want neither", fixture.prompts, runner.calls)
			}
		})
	}
}

func TestSetCodeRunnerIgnoresMissingParts(t *testing.T) {
	var executor *Executor
	executor.SetCodeRunner(&fakeCodeRunner{})
	fixture := newGateFixture(t, capabilitytest.Report(t, "code-interpreter"), true)
	fixture.executor.SetCodeRunner(nil)
	if _, err := fixture.call("code_execute", codeArguments, "code-interpreter"); err == nil {
		t.Fatal("a nil runner must leave the tool unavailable")
	}
	bare := &Executor{}
	bare.SetCodeRunner(&fakeCodeRunner{})
}

func TestCodeExecuteNeverReRunsAReplayedRequest(t *testing.T) {
	fixture := newGateFixture(t, capabilitytest.Report(t, "code-interpreter"), true)
	runner := &fakeCodeRunner{output: json.RawMessage(`{"result_class":"ok"}`)}
	fixture.executor.SetCodeRunner(runner)
	if _, err := fixture.call("code_execute", codeArguments, "code-interpreter"); err != nil {
		t.Fatalf("first call: %v", err)
	}
	_, err := fixture.call("code_execute", codeArguments, "code-interpreter")
	if code, ok := toolbroker.CodeOf(err); !ok || code != toolbroker.ResultExecutionFailed {
		t.Fatalf("replayed call error = %v, want execution_failed", err)
	}
	if runner.calls != 1 {
		t.Fatalf("runner calls = %d, want the program to run once", runner.calls)
	}
}

func TestCodeExecuteIsNeverOfferedToPublicPrincipals(t *testing.T) {
	fixture := newGateFixture(t, capabilitytest.Report(t, "code-interpreter"), true)
	runner := &fakeCodeRunner{output: json.RawMessage(`{}`)}
	fixture.executor.SetCodeRunner(runner)
	_, err := fixture.executor.Execute(context.Background(), &runtimeadk.BuiltinToolRequest{
		RequestID: "call-public", TurnID: "turn-1", SessionID: "session-1", PrincipalID: "visitor-1", PrincipalClass: principal.ClassPublic,
		ToolName: "code_execute", ToolVersion: "v1", Arguments: json.RawMessage(codeArguments),
		Capabilities: []string{"code-interpreter"}, Trust: string(approval.TrustDerivedUntrusted),
		IdempotencyKey: "test/public", EventSequence: 1, EventInvocation: "inv-1", EventBranch: "main", EventAuthor: "agent",
	})
	if code, ok := toolbroker.CodeOf(err); !ok || code != toolbroker.ResultToolTierDenied {
		t.Fatalf("code_execute for a public principal = %v, want tool_tier_denied", err)
	}
	if runner.calls != 0 || fixture.prompts != 0 {
		t.Fatalf("runner calls = %d, prompts = %d; want neither", runner.calls, fixture.prompts)
	}
}
