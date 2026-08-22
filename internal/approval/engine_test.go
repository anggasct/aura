package approval

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
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
	})
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
	if code, ok := CodeOf(err); !ok || code != ErrorCodePolicyDenied {
		t.Fatalf("CodeOf(%v) = %q, %v; want policy_denied", err, code, ok)
	}
}

func TestEvaluateTrustRestriction(t *testing.T) {
	engine := newTestEngine(t)
	request := testRequest("read_file")
	request.Trust = TrustUntrustedExternal
	_, err := engine.Evaluate(context.Background(), &request)
	if code, ok := CodeOf(err); !ok || code != ErrorCodePolicyDenied {
		t.Fatalf("CodeOf(%v) = %q, %v; want policy_denied for disallowed trust", err, code, ok)
	}
}

func TestEvaluateMissingCapability(t *testing.T) {
	engine := newTestEngine(t)
	request := testRequest("read_file")
	request.Capabilities = nil
	_, err := engine.Evaluate(context.Background(), &request)
	if code, ok := CodeOf(err); !ok || code != ErrorCodeCapabilityUnavailable {
		t.Fatalf("CodeOf(%v) = %q, %v; want capability_unavailable", err, code, ok)
	}
}

func TestEvaluateInvalidTrustLabel(t *testing.T) {
	engine := newTestEngine(t)
	request := testRequest("read_file")
	request.Trust = TrustLabel("spoofed")
	_, err := engine.Evaluate(context.Background(), &request)
	if code, ok := CodeOf(err); !ok || code != ErrorCodePolicyDenied {
		t.Fatalf("CodeOf(%v) = %q, %v; want policy_denied for invalid trust", err, code, ok)
	}
}

func TestApprovalOperationsRejectNilContext(t *testing.T) {
	var handlerCalls int
	engine, err := NewEngine(testPolicy(), func(_ context.Context, request ToolRequest, _ Constraints) (ToolResult, error) {
		handlerCalls++
		return ToolResult{ToolName: request.ToolName}, nil
	})
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
			} else if code, ok := CodeOf(err); !ok || code != ErrorCodeInvalidArgument {
				t.Fatalf("CodeOf(%v) = %q, %v; want invalid_argument", err, code, ok)
			}
		})
	}
	if handlerCalls != 0 {
		t.Fatalf("handler calls = %d, want zero for nil contexts", handlerCalls)
	}
}

// Every invocation passes through policy exactly once; execution is
// only reachable with a grant minted after policy passed, and reusing a
// grant fails.
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

	// The one-shot nonce is consumed: the same grant cannot run twice.
	if _, err := engine.Execute(context.Background(), &request, &grant); err == nil {
		t.Fatal("expected the consumed grant to be rejected")
	} else if code, ok := CodeOf(err); !ok || code != ErrorCodeApprovalInvalid {
		t.Fatalf("CodeOf(%v) = %q, %v; want approval_invalid", err, code, ok)
	}
}

func TestExecuteDeniedToolCannotGetGrant(t *testing.T) {
	engine := newTestEngine(t)
	_, err := engine.Grant(context.Background(), ptr(testRequest("rm_rf")), time.Minute)
	if code, ok := CodeOf(err); !ok || code != ErrorCodePolicyDenied {
		t.Fatalf("CodeOf(%v) = %q, %v; want policy_denied", err, code, ok)
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
	if code, ok := CodeOf(err); !ok || code != ErrorCodeApprovalInvalid {
		t.Fatalf("CodeOf(%v) = %q, %v; want approval_invalid", err, code, ok)
	}
}

// Argument, scope, expiry, or nonce changes invalidate a grant.
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
			if code, ok := CodeOf(err); !ok || code != ErrorCodeApprovalInvalid {
				t.Fatalf("CodeOf(%v) = %q, %v; want approval_invalid", err, code, ok)
			}
		})
	}
}

func TestExecuteRejectsConstraintMutations(t *testing.T) {
	var handlerCalls int
	engine, err := NewEngine(testPolicy(), func(_ context.Context, request ToolRequest, _ Constraints) (ToolResult, error) {
		handlerCalls++
		return ToolResult{ToolName: request.ToolName}, nil
	})
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
			} else if code, ok := CodeOf(err); !ok || code != ErrorCodeApprovalInvalid {
				t.Fatalf("CodeOf(%v) = %q, %v; want approval_invalid", err, code, ok)
			}
		})
	}
	if handlerCalls != 0 {
		t.Fatalf("handler calls = %d, want zero for mutated grants", handlerCalls)
	}
}

// An argument change in the request itself (tampered between grant
// and execute) invalidates the grant, because the hash no longer matches.
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
	if code, ok := CodeOf(err); !ok || code != ErrorCodeApprovalInvalid {
		t.Fatalf("CodeOf(%v) = %q, %v; want approval_invalid for tampered arguments", err, code, ok)
	}
}

// A capability change in the request invalidates the grant, so a skill or
// MCP server whose granted capabilities changed requires a new trust
// decision before it can execute again.
func TestExecuteRejectsCapabilityChange(t *testing.T) {
	engine := newTestEngine(t)
	request := testRequest("read_file")
	grant, err := engine.Grant(context.Background(), &request, time.Minute)
	if err != nil {
		t.Fatalf("Grant: %v", err)
	}

	escalated := request
	escalated.Capabilities = append(escalated.Capabilities, "sys-admin")
	_, err = engine.Execute(context.Background(), &escalated, &grant)
	if code, ok := CodeOf(err); !ok || code != ErrorCodeApprovalInvalid {
		t.Fatalf("CodeOf(%v) = %q, %v; want approval_invalid for capability change", err, code, ok)
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

// Untrusted and derived content is always data — it cannot change
// policy or the approval outcome. The policy is immutable after
// construction and no request field can mutate it; an untrusted request
// with a policy-approved tool still evaluates under policy constraints.
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

	// Untrusted content cannot escalate to an approval-required tool.
	escalation := testRequest("exec")
	escalation.Trust = TrustUntrustedExternal
	_, err = engine.Evaluate(context.Background(), &escalation)
	if code, ok := CodeOf(err); !ok || code != ErrorCodePolicyDenied {
		t.Fatalf("CodeOf(%v) = %q, %v; want policy_denied for untrusted exec", err, code, ok)
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
	if _, err := NewEngine(testPolicy(), nil); err == nil {
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
	})
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

	// Nil request on every entry point.
	if _, err := engine.Evaluate(context.Background(), nil); !isInvalidArgument(err) {
		t.Errorf("Evaluate(nil) = %v, want invalid_argument", err)
	}
	if _, err := engine.Grant(context.Background(), nil, time.Minute); !isInvalidArgument(err) {
		t.Errorf("Grant(nil) = %v, want invalid_argument", err)
	}
	if _, err := engine.Execute(context.Background(), nil, &ApprovalGrant{}); !isInvalidArgument(err) {
		t.Errorf("Execute(nil request) = %v, want invalid_argument", err)
	}

	// Nil grant on Execute.
	grant, err := engine.Grant(context.Background(), &request, time.Minute)
	if err != nil {
		t.Fatalf("Grant: %v", err)
	}
	if _, err := engine.Execute(context.Background(), &request, nil); !isInvalidArgument(err) {
		t.Errorf("Execute(nil grant) = %v, want invalid_argument", err)
	}
	_ = grant

	// Nil grant on ValidFor.
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
	code, ok := CodeOf(err)
	return ok && code == ErrorCodeInvalidArgument
}

func TestErrorsAreTyped(t *testing.T) {
	err := Errorf(ErrorCodePolicyDenied, "denied: %s", "x")
	if err == nil {
		t.Fatal("nil error")
	}
	if code, ok := CodeOf(err); !ok || code != ErrorCodePolicyDenied {
		t.Fatalf("CodeOf = %q, %v", code, ok)
	}
	if !errors.Is(err, err) {
		t.Fatal("errors.Is on itself must hold")
	}
	if strings.TrimSpace(err.Error()) == "" {
		t.Fatal("error message must not be empty")
	}
}
