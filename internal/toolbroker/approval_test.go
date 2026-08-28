package toolbroker

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/approval"
)

func TestBrokerApprovalDeciderAcceptExecutes(t *testing.T) {
	executor := newBrokerEffectExecutor(t)
	calls := 0
	var approvals []string
	broker, err := New(&Options{
		Effects: executor,
		Adapters: map[string]Adapter{
			"exec@v1": func(context.Context, *ToolRequest, approval.Constraints) (ToolResult, error) {
				calls++
				return ToolResult{Output: json.RawMessage(`{"ok":true}`)}, nil
			},
		},
		ApprovalDecider: func(_ context.Context, prompt *ApprovalPrompt) (bool, error) {
			return true, nil
		},
		Observer: func(_ context.Context, observation Observation) {
			approvals = append(approvals, observation.Approval)
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	request := brokerRequest("exec", `{"command":["true"]}`, "exec-linux")
	request.EventSequence = 1
	result, err := broker.Execute(context.Background(), request)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if calls != 1 || result.Class != ResultOK {
		t.Fatalf("calls = %d, result = %+v", calls, result)
	}
	if len(approvals) != 1 || approvals[0] != ApprovalApproved {
		t.Fatalf("approval states = %v, want approved", approvals)
	}
}

func TestBrokerApprovalDeciderRejectBlocksExecution(t *testing.T) {
	executor := newBrokerEffectExecutor(t)
	calls := 0
	broker, err := New(&Options{
		Effects: executor,
		Adapters: map[string]Adapter{
			"exec@v1": func(context.Context, *ToolRequest, approval.Constraints) (ToolResult, error) {
				calls++
				return ToolResult{Output: json.RawMessage(`{}`)}, nil
			},
		},
		ApprovalDecider: func(_ context.Context, _ *ApprovalPrompt) (bool, error) {
			return false, nil
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	request := brokerRequest("exec", `{"command":["true"]}`, "exec-linux")
	request.EventSequence = 1
	if _, err := broker.Execute(context.Background(), request); classOf(err) != ResultApprovalRequired {
		t.Fatalf("Execute error = %v, want approval_required", err)
	}
	if calls != 0 {
		t.Fatalf("adapter calls = %d, want 0 after rejection", calls)
	}
}

func TestBrokerApprovalDeciderErrorFailsClosed(t *testing.T) {
	executor := newBrokerEffectExecutor(t)
	broker, err := New(&Options{
		Effects: executor,
		Adapters: map[string]Adapter{
			"exec@v1": func(context.Context, *ToolRequest, approval.Constraints) (ToolResult, error) {
				t.Fatal("adapter must not run when the decider errors")
				return ToolResult{}, nil
			},
		},
		ApprovalDecider: func(_ context.Context, _ *ApprovalPrompt) (bool, error) {
			return false, errors.New("prompt surface failed")
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	request := brokerRequest("exec", `{"command":["true"]}`, "exec-linux")
	request.EventSequence = 1
	_, err = broker.Execute(context.Background(), request)
	if classOf(err) != ResultPolicyDenied {
		t.Fatalf("Execute error = %v, want policy_denied", err)
	}
}

func TestBrokerApprovalPromptCarriesCanonicalScope(t *testing.T) {
	executor := newBrokerEffectExecutor(t)
	var prompted *ApprovalPrompt
	broker, err := New(&Options{
		Effects: executor,
		Adapters: map[string]Adapter{
			"exec@v1": func(context.Context, *ToolRequest, approval.Constraints) (ToolResult, error) {
				return ToolResult{Output: json.RawMessage(`{}`)}, nil
			},
		},
		Secrets: []string{"sk-canary-9f2a"},
		ApprovalDecider: func(_ context.Context, prompt *ApprovalPrompt) (bool, error) {
			prompted = prompt
			return false, nil
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	request := brokerRequest("exec", `{"command":["echo","sk-canary-9f2a"]}`, "exec-linux")
	request.EventSequence = 1
	if _, err := broker.Execute(context.Background(), request); classOf(err) != ResultApprovalRequired {
		t.Fatalf("Execute error = %v, want approval_required", err)
	}
	if prompted == nil {
		t.Fatal("decider was not prompted")
	}
	if prompted.ToolName != "exec" || prompted.ToolVersion != "v1" || prompted.SessionID != "session-1" || prompted.PrincipalID != "owner-1" {
		t.Fatalf("prompt identity = %+v", prompted)
	}
	if strings.Contains(prompted.Arguments, "sk-canary-9f2a") || !strings.Contains(prompted.Arguments, "[REDACTED]") {
		t.Fatalf("prompt arguments leak secrets: %s", prompted.Arguments)
	}
	if prompted.Arguments != `{"command":["echo","[REDACTED]"]}` {
		t.Fatalf("prompt arguments = %s, want canonical redacted form", prompted.Arguments)
	}
	if prompted.PolicyVersion == "" || prompted.ReasonCode == "" {
		t.Fatalf("prompt policy fields = %+v", prompted)
	}
	if prompted.MaxOutputBytes <= 0 || prompted.Timeout <= 0 {
		t.Fatalf("prompt constraints = %+v", prompted)
	}
	if !time.Now().Before(prompted.ExpiresAt) {
		t.Fatalf("prompt expiry %s is not in the future", prompted.ExpiresAt)
	}
}

func TestBrokerApprovalPromptBoundsDisplayArguments(t *testing.T) {
	executor := newBrokerEffectExecutor(t)
	var prompted *ApprovalPrompt
	broker, err := New(&Options{
		Effects: executor,
		Adapters: map[string]Adapter{
			"exec@v1": func(context.Context, *ToolRequest, approval.Constraints) (ToolResult, error) {
				return ToolResult{Output: json.RawMessage(`{}`)}, nil
			},
		},
		ApprovalDecider: func(_ context.Context, prompt *ApprovalPrompt) (bool, error) {
			prompted = prompt
			return false, nil
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	long := strings.Repeat("a", 64*1024)
	request := brokerRequest("exec", `{"command":["`+long+`"]}`, "exec-linux")
	request.EventSequence = 1
	if _, err := broker.Execute(context.Background(), request); classOf(err) != ResultApprovalRequired {
		t.Fatalf("Execute error = %v, want approval_required", err)
	}
	if prompted == nil || len(prompted.Arguments) > maxApprovalDisplayBytes+len("...[truncated]") {
		t.Fatalf("prompt arguments are not bounded: %d bytes", len(prompted.Arguments))
	}
	if !strings.HasSuffix(prompted.Arguments, "...[truncated]") {
		t.Fatalf("bounded arguments = %q, want truncation marker", prompted.Arguments)
	}
}

func TestBrokerApprovedGrantIsOneShot(t *testing.T) {
	executor := newBrokerEffectExecutor(t)
	calls := 0
	broker, err := New(&Options{
		Effects: executor,
		Adapters: map[string]Adapter{
			"exec@v1": func(context.Context, *ToolRequest, approval.Constraints) (ToolResult, error) {
				calls++
				return ToolResult{Output: json.RawMessage(`{}`)}, nil
			},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	request := brokerRequest("exec", `{"command":["true"]}`, "exec-linux")
	request.EventSequence = 1
	grant, err := broker.Grant(context.Background(), request, time.Minute)
	if err != nil {
		t.Fatalf("Grant: %v", err)
	}
	request.Approval = &grant
	if _, err := broker.Execute(context.Background(), request); err != nil {
		t.Fatalf("Execute with grant: %v", err)
	}
	request.EventSequence = 2
	if _, err := broker.Execute(context.Background(), request); classOf(err) != ResultPolicyDenied {
		t.Fatalf("replayed grant error = %v, want policy_denied", err)
	}
	if calls != 1 {
		t.Fatalf("adapter calls = %d, want 1", calls)
	}
}
