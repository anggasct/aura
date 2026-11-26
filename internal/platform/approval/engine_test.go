package approval

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/fault"
)

func testPolicy() Policy {
	return Policy{
		Version: "v1",
		Rules: map[string]Rule{
			"read_file": {
				ToolName:             "read_file",
				RequiredCapabilities: []string{"file-read"},
				AllowedTrust:         []TrustLabel{TrustOwnerInput, TrustTrustedConfiguration},
				Constraints:          Constraints{MaxOutputBytes: 1 << 20},
			},
			"exec": {
				ToolName:             "exec",
				RequiresApproval:     true,
				RequiredCapabilities: []string{"exec"},
				AllowedTrust:         []TrustLabel{TrustOwnerInput},
				Constraints:          Constraints{AllowNetwork: true, Timeout: 30 * time.Second},
			},
			"web_fetch": {
				ToolName:             "web_fetch",
				RequiredCapabilities: []string{"web"},
				Constraints:          Constraints{AllowNetwork: true, MaxOutputBytes: 1 << 20},
			},
		},
	}
}

var testHostCapabilities = []string{"file-read", "exec", "web"}

func testRequest(tool string) ToolRequest {
	return ToolRequest{
		RequestID:      "req-1",
		TurnID:         "turn-1",
		SessionID:      "session-1",
		PrincipalID:    "owner",
		ToolName:       tool,
		Arguments:      json.RawMessage(`{"path":"/tmp/x"}`),
		Capabilities:   []string{"file-read", "exec", "web"},
		Trust:          TrustOwnerInput,
		IdempotencyKey: "idem-1",
	}
}

func ptr[T any](v T) *T { return &v }

func newTestEngine(t *testing.T) *Engine {
	t.Helper()
	engine, err := NewEngine(testPolicy(), func(_ context.Context, request ToolRequest, constraints Constraints) (ToolResult, error) {
		return ToolResult{ToolName: request.ToolName, Output: json.RawMessage(`{"ok":true}`)}, nil
	}, testHostCapabilities)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	return engine
}

func TestEvaluateAllow(t *testing.T) {
	engine := newTestEngine(t)
	request := testRequest("read_file")
	decision, err := engine.Evaluate(context.Background(), &request)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if decision.Outcome != OutcomeAllow {
		t.Errorf("Outcome = %q, want allow", decision.Outcome)
	}
	if decision.PolicyVersion != "v1" {
		t.Errorf("PolicyVersion = %q, want v1", decision.PolicyVersion)
	}
	if decision.Constraints.MaxOutputBytes != 1<<20 {
		t.Errorf("Constraints = %+v", decision.Constraints)
	}
}

func TestEvaluateRequireApproval(t *testing.T) {
	engine := newTestEngine(t)
	decision, err := engine.Evaluate(context.Background(), ptr(testRequest("exec")))
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if decision.Outcome != OutcomeRequireApproval {
		t.Errorf("Outcome = %q, want require_approval", decision.Outcome)
	}
}

func TestEvaluateUnknownToolDenied(t *testing.T) {
	engine := newTestEngine(t)
	_, err := engine.Evaluate(context.Background(), ptr(testRequest("rm_rf")))
	if code, ok := fault.CodeOf[ErrorCode](err); !ok || code != ErrorCodePolicyDenied {
		t.Fatalf("fault.CodeOf[ErrorCode](%v) = %q, %v; want policy_denied", err, code, ok)
	}
}

func TestEvaluateTrustRestriction(t *testing.T) {
	engine := newTestEngine(t)
	request := testRequest("read_file")
	request.Trust = TrustUntrustedExternal
	_, err := engine.Evaluate(context.Background(), &request)
	if code, ok := fault.CodeOf[ErrorCode](err); !ok || code != ErrorCodePolicyDenied {
		t.Fatalf("fault.CodeOf[ErrorCode](%v) = %q, %v; want policy_denied for disallowed trust", err, code, ok)
	}
}

func TestEvaluateChecksTheHostCapabilitySet(t *testing.T) {
	handler := func(context.Context, ToolRequest, Constraints) (ToolResult, error) { return ToolResult{}, nil }
	cases := []struct {
		name      string
		host      []string
		tool      string
		claimed   []string
		wantAllow bool
	}{
		{name: "host holds the required capability", host: []string{"file-read"}, tool: "read_file", claimed: []string{"file-read"}, wantAllow: true},
		{name: "request claims nothing", host: []string{"file-read"}, tool: "read_file", claimed: nil, wantAllow: true},
		{name: "host has no capabilities", host: nil, tool: "read_file", claimed: []string{"file-read"}},
		{name: "host lacks the required capability while the request claims it", host: []string{"exec"}, tool: "read_file", claimed: []string{"file-read"}},
		{name: "host lacks the required capability and the request claims nothing", host: []string{"exec"}, tool: "read_file", claimed: nil},
		{name: "request claims a capability the host lacks", host: []string{"file-read"}, tool: "read_file", claimed: []string{"file-read", "exec"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			engine, err := NewEngine(testPolicy(), handler, tc.host)
			if err != nil {
				t.Fatalf("NewEngine: %v", err)
			}
			request := testRequest(tc.tool)
			request.Capabilities = tc.claimed
			decision, err := engine.Evaluate(context.Background(), &request)
			if tc.wantAllow {
				if err != nil || decision.Outcome != OutcomeAllow {
					t.Fatalf("Evaluate = %+v, %v; want allow", decision, err)
				}
				return
			}
			if code, ok := fault.CodeOf[ErrorCode](err); !ok || code != ErrorCodeCapabilityUnavailable {
				t.Fatalf("fault.CodeOf[ErrorCode](%v) = %q, %v; want capability_unavailable", err, code, ok)
			}
			if _, err := engine.Grant(context.Background(), &request, time.Minute); err == nil {
				t.Fatal("Grant succeeded for a request the host cannot satisfy")
			}
		})
	}
}

func TestEngineDoesNotAliasTheHostCapabilitySlice(t *testing.T) {
	host := []string{"file-read"}
	engine, err := NewEngine(testPolicy(), func(context.Context, ToolRequest, Constraints) (ToolResult, error) { return ToolResult{}, nil }, host)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	host[0] = "other"
	request := testRequest("read_file")
	request.Capabilities = []string{"file-read"}
	if _, err := engine.Evaluate(context.Background(), &request); err != nil {
		t.Fatalf("Evaluate after the caller mutated its slice: %v", err)
	}
}

func TestEvaluateInvalidTrustLabel(t *testing.T) {
	engine := newTestEngine(t)
	request := testRequest("read_file")
	request.Trust = TrustLabel("spoofed")
	_, err := engine.Evaluate(context.Background(), &request)
	if code, ok := fault.CodeOf[ErrorCode](err); !ok || code != ErrorCodePolicyDenied {
		t.Fatalf("fault.CodeOf[ErrorCode](%v) = %q, %v; want policy_denied for invalid trust", err, code, ok)
	}
}

func TestApprovalOperationsRejectNilContext(t *testing.T) {
	var handlerCalls int
	engine, err := NewEngine(testPolicy(), func(_ context.Context, request ToolRequest, _ Constraints) (ToolResult, error) {
		handlerCalls++
		return ToolResult{ToolName: request.ToolName}, nil
	}, testHostCapabilities)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	request := testRequest("read_file")
	grant, err := engine.Grant(context.Background(), &request, time.Minute)
	if err != nil {
		t.Fatalf("Grant: %v", err)
	}
	var nilContext context.Context
	cases := []struct {
		name string
		run  func() error
	}{
		{name: "evaluate", run: func() error {
			_, err := engine.Evaluate(nilContext, &request)
			return err
		}},
		{name: "grant", run: func() error {
			_, err := engine.Grant(nilContext, &request, time.Minute)
			return err
		}},
		{name: "validate and consume", run: func() error {
			return engine.ValidateAndConsume(nilContext, &request, &grant)
		}},
		{name: "execute", run: func() error {
			_, err := engine.Execute(nilContext, &request, &grant)
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.run(); err == nil {
				t.Fatal("operation accepted a nil context")
			} else if code, ok := fault.CodeOf[ErrorCode](err); !ok || code != ErrorCodeInvalidArgument {
				t.Fatalf("fault.CodeOf[ErrorCode](%v) = %q, %v; want invalid_argument", err, code, ok)
			}
		})
	}
	if handlerCalls != 0 {
		t.Fatalf("handler calls = %d, want zero for nil contexts", handlerCalls)
	}
}

func TestExecuteRequiresGrantFromPolicy(t *testing.T) {
	engine := newTestEngine(t)
	request := testRequest("read_file")

	grant, err := engine.Grant(context.Background(), &request, time.Minute)
	if err != nil {
		t.Fatalf("Grant: %v", err)
	}
	result, err := engine.Execute(context.Background(), &request, &grant)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.ToolName != "read_file" {
		t.Errorf("ToolName = %q", result.ToolName)
	}

	if _, err := engine.Execute(context.Background(), &request, &grant); err == nil {
		t.Fatal("expected the consumed grant to be rejected")
	} else if code, ok := fault.CodeOf[ErrorCode](err); !ok || code != ErrorCodeApprovalInvalid {
		t.Fatalf("fault.CodeOf[ErrorCode](%v) = %q, %v; want approval_invalid", err, code, ok)
	}
}

func TestExecuteDeniedToolCannotGetGrant(t *testing.T) {
	engine := newTestEngine(t)
	_, err := engine.Grant(context.Background(), ptr(testRequest("rm_rf")), time.Minute)
	if code, ok := fault.CodeOf[ErrorCode](err); !ok || code != ErrorCodePolicyDenied {
		t.Fatalf("fault.CodeOf[ErrorCode](%v) = %q, %v; want policy_denied", err, code, ok)
	}
}

func TestExecuteForgedGrantRejected(t *testing.T) {
	engine := newTestEngine(t)
	request := testRequest("read_file")
	forged := ApprovalGrant{
		GrantID:       "forged",
		PrincipalID:   request.PrincipalID,
		SessionID:     request.SessionID,
		ToolName:      request.ToolName,
		ArgumentsHash: HashArguments(request.Arguments),
		PolicyVersion: "v1",
		ExpiresAt:     time.Now().Add(time.Minute),
		Nonce:         "forged-nonce",
	}
	_, err := engine.Execute(context.Background(), &request, &forged)
	if err == nil {
		t.Fatal("expected a forged grant to be rejected")
	}
	if code, ok := fault.CodeOf[ErrorCode](err); !ok || code != ErrorCodeApprovalInvalid {
		t.Fatalf("fault.CodeOf[ErrorCode](%v) = %q, %v; want approval_invalid", err, code, ok)
	}
}

func TestGrantInvalidatedByFieldChanges(t *testing.T) {
	engine := newTestEngine(t)
	request := testRequest("read_file")
	grant, err := engine.Grant(context.Background(), &request, time.Minute)
	if err != nil {
		t.Fatalf("Grant: %v", err)
	}

	mutate := func(f func(g *ApprovalGrant)) func() error {
		return func() error {
			changed := grant
			f(&changed)
			_, err := engine.Execute(context.Background(), &request, &changed)
			return err
		}
	}

	cases := []struct {
		name string
		run  func() error
	}{
		{name: "arguments changed", run: mutate(func(g *ApprovalGrant) { g.ArgumentsHash = "different" })},
		{name: "capabilities changed", run: mutate(func(g *ApprovalGrant) { g.CapabilitiesHash = "different" })},
		{name: "session changed", run: mutate(func(g *ApprovalGrant) { g.SessionID = "session-2" })},
		{name: "principal changed", run: mutate(func(g *ApprovalGrant) { g.PrincipalID = "attacker" })},
		{name: "tool changed", run: mutate(func(g *ApprovalGrant) { g.ToolName = "exec" })},
		{name: "policy version changed", run: mutate(func(g *ApprovalGrant) { g.PolicyVersion = "v0" })},
		{name: "nonce changed", run: mutate(func(g *ApprovalGrant) { g.Nonce = "other-nonce" })},
		{name: "expired", run: mutate(func(g *ApprovalGrant) { g.ExpiresAt = time.Now().Add(-time.Second) })},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.run()
			if err == nil {
				t.Fatal("expected the mutated grant to be rejected")
			}
			if code, ok := fault.CodeOf[ErrorCode](err); !ok || code != ErrorCodeApprovalInvalid {
				t.Fatalf("fault.CodeOf[ErrorCode](%v) = %q, %v; want approval_invalid", err, code, ok)
			}
		})
	}
}

func TestExecuteRejectsConstraintMutations(t *testing.T) {
	var handlerCalls int
	engine, err := NewEngine(testPolicy(), func(_ context.Context, request ToolRequest, _ Constraints) (ToolResult, error) {
		handlerCalls++
		return ToolResult{ToolName: request.ToolName}, nil
	}, testHostCapabilities)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	request := testRequest("exec")
	cases := []struct {
		name   string
		mutate func(*ApprovalGrant)
	}{
		{name: "network", mutate: func(grant *ApprovalGrant) { grant.Constraints.AllowNetwork = false }},
		{name: "output limit", mutate: func(grant *ApprovalGrant) { grant.Constraints.MaxOutputBytes = 1 }},
		{name: "timeout", mutate: func(grant *ApprovalGrant) { grant.Constraints.Timeout = time.Second }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			grant, err := engine.Grant(context.Background(), &request, time.Minute)
			if err != nil {
				t.Fatalf("Grant: %v", err)
			}
			tc.mutate(&grant)
			if _, err := engine.Execute(context.Background(), &request, &grant); err == nil {
				t.Fatal("Execute accepted mutated constraints")
			} else if code, ok := fault.CodeOf[ErrorCode](err); !ok || code != ErrorCodeApprovalInvalid {
				t.Fatalf("fault.CodeOf[ErrorCode](%v) = %q, %v; want approval_invalid", err, code, ok)
			}
		})
	}
	if handlerCalls != 0 {
		t.Fatalf("handler calls = %d, want zero for mutated grants", handlerCalls)
	}
}

func TestExecuteRejectsTamperedArguments(t *testing.T) {
	engine := newTestEngine(t)
	request := testRequest("read_file")
	grant, err := engine.Grant(context.Background(), &request, time.Minute)
	if err != nil {
		t.Fatalf("Grant: %v", err)
	}

	tampered := request
	tampered.Arguments = json.RawMessage(`{"path":"/etc/passwd"}`)
	_, err = engine.Execute(context.Background(), &tampered, &grant)
	if code, ok := fault.CodeOf[ErrorCode](err); !ok || code != ErrorCodeApprovalInvalid {
		t.Fatalf("fault.CodeOf[ErrorCode](%v) = %q, %v; want approval_invalid for tampered arguments", err, code, ok)
	}
}

func TestExecuteRejectsCapabilityChange(t *testing.T) {
	engine, err := NewEngine(testPolicy(), func(context.Context, ToolRequest, Constraints) (ToolResult, error) { return ToolResult{}, nil }, append(slices.Clone(testHostCapabilities), "sys-admin"))
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	request := testRequest("read_file")
	grant, err := engine.Grant(context.Background(), &request, time.Minute)
	if err != nil {
		t.Fatalf("Grant: %v", err)
	}

	escalated := request
	escalated.Capabilities = append(escalated.Capabilities, "sys-admin")
	_, err = engine.Execute(context.Background(), &escalated, &grant)
	if code, ok := fault.CodeOf[ErrorCode](err); !ok || code != ErrorCodeApprovalInvalid {
		t.Fatalf("fault.CodeOf[ErrorCode](%v) = %q, %v; want approval_invalid for capability change", err, code, ok)
	}
}

func TestHashCapabilitiesIsOrderIndependent(t *testing.T) {
	first := HashCapabilities([]string{"web", "file-read", "exec"})
	second := HashCapabilities([]string{"exec", "file-read", "web"})
	if first != second {
		t.Fatalf("hash depends on ordering: %q != %q", first, second)
	}
	if HashCapabilities([]string{"web", "file-read"}) == first {
		t.Fatal("different sets must hash differently")
	}
	if HashCapabilities([]string{"exec", "exec", "file-read", "web"}) == first {
		t.Fatal("duplicate entries must hash differently from the deduplicated set")
	}
}

func TestUntrustedContentCannotModifyPolicy(t *testing.T) {
	engine := newTestEngine(t)
	request := testRequest("web_fetch")
	request.Trust = TrustUntrustedExternal
	decision, err := engine.Evaluate(context.Background(), &request)
	if err != nil {
		t.Fatalf("Evaluate untrusted web_fetch: %v", err)
	}
	if decision.Outcome != OutcomeAllow {
		t.Errorf("Outcome = %q, want allow for a tool without trust restriction", decision.Outcome)
	}
	if engine.PolicyVersion() != "v1" {
		t.Errorf("PolicyVersion changed to %q", engine.PolicyVersion())
	}

	escalation := testRequest("exec")
	escalation.Trust = TrustUntrustedExternal
	_, err = engine.Evaluate(context.Background(), &escalation)
	if code, ok := fault.CodeOf[ErrorCode](err); !ok || code != ErrorCodePolicyDenied {
		t.Fatalf("fault.CodeOf[ErrorCode](%v) = %q, %v; want policy_denied for untrusted exec", err, code, ok)
	}
}

func TestPolicyValidate(t *testing.T) {
	valid := testPolicy()
	if err := valid.Validate(); err != nil {
		t.Errorf("valid policy rejected: %v", err)
	}

	bad := []Policy{
		{Version: "", Rules: valid.Rules},
		{Version: "v1", Rules: map[string]Rule{"x": {ToolName: "y"}}},
		{Version: "v1", Rules: map[string]Rule{"x": {ToolName: "x", AllowedTrust: []TrustLabel{"spoofed"}}}},
		{Version: "v1", Rules: map[string]Rule{"x": {ToolName: "x", Constraints: Constraints{MaxOutputBytes: -1}}}},
	}
	for i, policy := range bad {
		if err := policy.Validate(); err == nil {
			t.Errorf("bad policy %d accepted", i)
		}
	}
}

func TestPolicyValidateReportsEveryRule(t *testing.T) {
	policy := Policy{
		Version: "",
		Rules: map[string]Rule{
			"one": {ToolName: "one", Constraints: Constraints{MaxOutputBytes: -1}},
			"two": {ToolName: "two", AllowedTrust: []TrustLabel{"spoofed"}},
		},
	}
	err := policy.Validate()
	if err == nil {
		t.Fatal("expected the invalid policy to be rejected")
	}
	for _, want := range []string{"one", "two"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("aggregate error omits rule %q: %v", want, err)
		}
	}
}

func TestNewEngineRejectsNilHandler(t *testing.T) {
	if _, err := NewEngine(testPolicy(), nil, testHostCapabilities); err == nil {
		t.Fatal("expected nil handler to be rejected")
	}
}

func TestExecuteConcurrentGrants(t *testing.T) {
	engine := newTestEngine(t)
	request := testRequest("read_file")

	var wg sync.WaitGroup
	errs := make([]error, 40)
	for i := range 40 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			grant, err := engine.Grant(context.Background(), &request, time.Minute)
			if err != nil {
				errs[i] = err
				return
			}
			_, errs[i] = engine.Execute(context.Background(), &request, &grant)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent grant %d: %v", i, err)
		}
	}
}

func TestHandlerReceivesConstraints(t *testing.T) {
	var got Constraints
	engine, err := NewEngine(testPolicy(), func(_ context.Context, request ToolRequest, constraints Constraints) (ToolResult, error) {
		got = constraints
		return ToolResult{ToolName: request.ToolName}, nil
	}, testHostCapabilities)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	request := testRequest("exec")
	grant, err := engine.Grant(context.Background(), &request, time.Minute)
	if err != nil {
		t.Fatalf("Grant: %v", err)
	}
	if _, err := engine.Execute(context.Background(), &request, &grant); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !got.AllowNetwork || got.Timeout != 30*time.Second {
		t.Errorf("handler constraints = %+v, want policy constraints", got)
	}
}

func TestGrantTTLMustBePositive(t *testing.T) {
	engine := newTestEngine(t)
	if _, err := engine.Grant(context.Background(), ptr(testRequest("read_file")), 0); err == nil {
		t.Fatal("expected zero ttl to be rejected")
	}
}

func TestTrustLabelHelpers(t *testing.T) {
	for _, label := range []TrustLabel{TrustOwnerInput, TrustTrustedConfiguration, TrustUntrustedExternal, TrustDerivedUntrusted} {
		if !label.Valid() {
			t.Errorf("%q should be valid", label)
		}
	}
	if TrustLabel("x").Valid() {
		t.Error("invalid label accepted")
	}
	if !TrustUntrustedExternal.IsUntrusted() || !TrustDerivedUntrusted.IsUntrusted() {
		t.Error("untrusted labels must report untrusted")
	}
	if TrustOwnerInput.IsUntrusted() || TrustTrustedConfiguration.IsUntrusted() {
		t.Error("trusted labels must not report untrusted")
	}
}

func TestNilArgumentsReturnInvalidArgument(t *testing.T) {
	engine := newTestEngine(t)
	request := testRequest("read_file")

	if _, err := engine.Evaluate(context.Background(), nil); !isInvalidArgument(err) {
		t.Errorf("Evaluate(nil) = %v, want invalid_argument", err)
	}
	if _, err := engine.Grant(context.Background(), nil, time.Minute); !isInvalidArgument(err) {
		t.Errorf("Grant(nil) = %v, want invalid_argument", err)
	}
	if _, err := engine.Execute(context.Background(), nil, &ApprovalGrant{}); !isInvalidArgument(err) {
		t.Errorf("Execute(nil request) = %v, want invalid_argument", err)
	}

	grant, err := engine.Grant(context.Background(), &request, time.Minute)
	if err != nil {
		t.Fatalf("Grant: %v", err)
	}
	if _, err := engine.Execute(context.Background(), &request, nil); !isInvalidArgument(err) {
		t.Errorf("Execute(nil grant) = %v, want invalid_argument", err)
	}
	_ = grant

	if err := (*ApprovalGrant)(nil).ValidFor(&request, "v1", time.Now()); !isInvalidArgument(err) {
		t.Errorf("ValidFor(nil grant) = %v, want invalid_argument", err)
	}
	if err := grant.ValidFor(nil, "v1", time.Now()); !isInvalidArgument(err) {
		t.Errorf("ValidFor(nil request) = %v, want invalid_argument", err)
	}
}

func isInvalidArgument(err error) bool {
	if err == nil {
		return false
	}
	code, ok := fault.CodeOf[ErrorCode](err)
	return ok && code == ErrorCodeInvalidArgument
}

func TestErrorsAreTyped(t *testing.T) {
	err := Errorf(ErrorCodePolicyDenied, "denied: %s", "x")
	if err == nil {
		t.Fatal("nil error")
	}
	if code, ok := fault.CodeOf[ErrorCode](err); !ok || code != ErrorCodePolicyDenied {
		t.Fatalf("CodeOf = %q, %v", code, ok)
	}
	if !errors.Is(err, err) {
		t.Fatal("errors.Is on itself must hold")
	}
	if strings.TrimSpace(err.Error()) == "" {
		t.Fatal("error message must not be empty")
	}
}

func TestEngineRegisterAndUnregisterRule(t *testing.T) {
	engine := newTestEngine(t)
	ctx := t.Context()

	req := testRequest("dynamic_tool")
	_, err := engine.Evaluate(ctx, &req)
	if err == nil {
		t.Fatal("expected evaluate to fail for unregistered tool")
	}

	rule := Rule{
		ToolName:             "dynamic_tool",
		RequiredCapabilities: []string{"exec"},
		AllowedTrust:         []TrustLabel{TrustOwnerInput},
		Constraints:          Constraints{MaxOutputBytes: 1024},
	}
	if err := engine.RegisterRule(&rule); err != nil {
		t.Fatalf("RegisterRule failed: %v", err)
	}

	dec, err := engine.Evaluate(ctx, &req)
	if err != nil {
		t.Fatalf("expected evaluate to succeed after registering rule: %v", err)
	}
	if dec.Outcome != OutcomeAllow {
		t.Fatalf("expected outcome allow, got %s", dec.Outcome)
	}

	engine.UnregisterRule("dynamic_tool")
	_, err = engine.Evaluate(ctx, &req)
	if err == nil {
		t.Fatal("expected evaluate to fail after unregistering rule")
	}
}

func TestEngineRegisterRulesIsAtomicOnFailure(t *testing.T) {
	engine := newTestEngine(t)
	ctx := t.Context()

	valid := Rule{
		ToolName:     "batch_tool_a",
		AllowedTrust: []TrustLabel{TrustOwnerInput},
		Constraints:  Constraints{MaxOutputBytes: 1024},
	}
	overwriteReadFile := Rule{
		ToolName:         "read_file",
		RequiresApproval: true,
		AllowedTrust:     []TrustLabel{TrustOwnerInput},
	}
	invalid := Rule{
		ToolName:    "batch_tool_b",
		Constraints: Constraints{MaxOutputBytes: -1},
	}

	if err := engine.RegisterRules([]Rule{overwriteReadFile, valid, invalid}); !fault.HasCode(err, ErrorCodeInvalidArgument) {
		t.Fatalf("RegisterRules with invalid entry err = %v, want %q", err, ErrorCodeInvalidArgument)
	}
	reqA := testRequest("batch_tool_a")
	if _, err := engine.Evaluate(ctx, &reqA); !fault.HasCode(err, ErrorCodePolicyDenied) {
		t.Fatalf("batch_tool_a was registered despite batch failure: %v", err)
	}
	reqRead := testRequest("read_file")
	dec, err := engine.Evaluate(ctx, &reqRead)
	if err != nil || dec.Outcome != OutcomeAllow {
		t.Fatalf("pre-existing read_file rule mutated: dec = %+v, err = %v", dec, err)
	}

	if err := engine.RegisterRules([]Rule{valid, valid}); !fault.HasCode(err, ErrorCodeInvalidArgument) {
		t.Fatalf("RegisterRules with duplicate tool name err = %v, want %q", err, ErrorCodeInvalidArgument)
	}
	if _, err := engine.Evaluate(ctx, &reqA); !fault.HasCode(err, ErrorCodePolicyDenied) {
		t.Fatalf("batch_tool_a was registered despite duplicate failure: %v", err)
	}

	second := Rule{
		ToolName:     "batch_tool_b",
		AllowedTrust: []TrustLabel{TrustOwnerInput},
		Constraints:  Constraints{MaxOutputBytes: 2048},
	}
	if err := engine.RegisterRules([]Rule{valid, second}); err != nil {
		t.Fatalf("RegisterRules valid batch: %v", err)
	}
	if dec, err := engine.Evaluate(ctx, &reqA); err != nil || dec.Outcome != OutcomeAllow {
		t.Fatalf("Evaluate batch_tool_a = %+v, %v", dec, err)
	}
	reqB := testRequest("batch_tool_b")
	if dec, err := engine.Evaluate(ctx, &reqB); err != nil || dec.Outcome != OutcomeAllow {
		t.Fatalf("Evaluate batch_tool_b = %+v, %v", dec, err)
	}
}

func TestEnginePolicyReturnsIsolatedDeepCopy(t *testing.T) {
	engine := newTestEngine(t)

	snapshot := engine.Policy()
	if snapshot.Version != "v1" {
		t.Fatalf("Policy().Version = %q, want v1", snapshot.Version)
	}
	if len(snapshot.Rules) != 3 {
		t.Fatalf("len(Policy().Rules) = %d, want 3", len(snapshot.Rules))
	}

	readRule := snapshot.Rules["read_file"]
	readRule.RequiredCapabilities[0] = "mutated-cap"
	readRule.AllowedTrust[0] = TrustUntrustedExternal
	snapshot.Rules["read_file"] = Rule{ToolName: "read_file", RequiresApproval: true}
	delete(snapshot.Rules, "exec")
	snapshot.Rules["injected"] = Rule{ToolName: "injected"}

	fresh := engine.Policy()
	if _, exists := fresh.Rules["injected"]; exists {
		t.Fatal("mutating returned Policy map leaked into engine")
	}
	if _, exists := fresh.Rules["exec"]; !exists {
		t.Fatal("deleting from returned Policy map removed rule from engine")
	}
	gotRead := fresh.Rules["read_file"]
	if gotRead.RequiresApproval {
		t.Fatal("overwriting rule in returned Policy map mutated engine rule")
	}
	if !slices.Equal(gotRead.RequiredCapabilities, []string{"file-read"}) {
		t.Fatalf("RequiredCapabilities mutated to %v", gotRead.RequiredCapabilities)
	}
	if !slices.Equal(gotRead.AllowedTrust, []TrustLabel{TrustOwnerInput, TrustTrustedConfiguration}) {
		t.Fatalf("AllowedTrust mutated to %v", gotRead.AllowedTrust)
	}

	caps := []string{"web"}
	trust := []TrustLabel{TrustOwnerInput}
	added := Rule{
		ToolName:             "custom_snapshot_tool",
		ToolVersion:          "v1",
		RequiredCapabilities: caps,
		AllowedTrust:         trust,
		Constraints:          Constraints{MaxOutputBytes: 4096},
	}
	if err := engine.RegisterRule(&added); err != nil {
		t.Fatalf("RegisterRule: %v", err)
	}
	caps[0] = "tampered"
	trust[0] = TrustUntrustedExternal

	afterAdd := engine.Policy()
	gotAdded, ok := afterAdd.Rules["custom_snapshot_tool"]
	if !ok {
		t.Fatal("newly registered rule missing from Policy()")
	}
	if !slices.Equal(gotAdded.RequiredCapabilities, []string{"web"}) {
		t.Fatalf("registered rule aliased caller caps: %v", gotAdded.RequiredCapabilities)
	}
	if !slices.Equal(gotAdded.AllowedTrust, []TrustLabel{TrustOwnerInput}) {
		t.Fatalf("registered rule aliased caller trust: %v", gotAdded.AllowedTrust)
	}

	engine.UnregisterRule("custom_snapshot_tool")
	if _, exists := engine.Policy().Rules["custom_snapshot_tool"]; exists {
		t.Fatal("unregistered rule still present in Policy()")
	}
}
