package toolbroker

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/tools"
)

type stubToolProvider struct {
	regs []ToolRegistration
}

func (p stubToolProvider) Tools() []ToolRegistration { return p.regs }

func sampleRegistration(name string) ToolRegistration {
	return ToolRegistration{
		Definition: tools.Definition{
			Name:    name,
			Version: tools.Version,
			Schema:  json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}},"required":["q"],"additionalProperties":false}`),
			Validator: func(raw json.RawMessage) (json.RawMessage, error) {
				return raw, nil
			},
		},
		Adapter: func(context.Context, *ToolRequest, approval.Constraints) (ToolResult, error) {
			return ToolResult{Class: ResultOK, Output: json.RawMessage(`{"ok":true}`)}, nil
		},
		Rule: approval.Rule{
			ToolName:    name,
			ToolVersion: tools.Version,
			AllowedTrust: []approval.TrustLabel{
				approval.TrustOwnerInput,
				approval.TrustTrustedConfiguration,
				approval.TrustDerivedUntrusted,
			},
			Constraints: approval.Constraints{
				MaxOutputBytes: 64 * 1024,
				Timeout:        30 * time.Second,
			},
		},
	}
}

func TestBrokerRegisterProviderRefusesNilAndValidatesEntries(t *testing.T) {
	broker, err := New(&Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := broker.RegisterProvider(nil); !fault.HasCode(err, ResultInvalidArgument) {
		t.Fatalf("nil provider err = %v, want %q", err, ResultInvalidArgument)
	}
	if err := broker.RegisterProvider(stubToolProvider{}); err != nil {
		t.Fatalf("empty provider err = %v", err)
	}

	valid := sampleRegistration("custom_tool")
	cases := map[string][]ToolRegistration{
		"nil adapter": {
			{Definition: valid.Definition, Rule: valid.Rule},
		},
		"empty name": {
			{Definition: tools.Definition{Version: tools.Version}, Adapter: valid.Adapter, Rule: valid.Rule},
		},
		"rule mismatch": {
			{
				Definition: valid.Definition,
				Adapter:    valid.Adapter,
				Rule:       approval.Rule{ToolName: "other_tool", ToolVersion: tools.Version},
			},
		},
		"duplicate key": {valid, valid},
	}
	for name, regs := range cases {
		t.Run(name, func(t *testing.T) {
			err := broker.RegisterProvider(stubToolProvider{regs: regs})
			if !fault.HasCode(err, ResultInvalidArgument) {
				t.Fatalf("err = %v, want %q", err, ResultInvalidArgument)
			}
		})
	}
}

func TestBrokerRegisterProviderIsAtomicOnFailure(t *testing.T) {
	broker, err := New(&Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	overwriting := sampleRegistration(tools.KBSearchToolName)
	overwriting.Rule.RequiresApproval = true

	invalidRule := sampleRegistration("broken_tool")
	invalidRule.Rule.Constraints.MaxOutputBytes = -1

	err = broker.RegisterProvider(stubToolProvider{
		regs: []ToolRegistration{overwriting, invalidRule},
	})
	if err == nil {
		t.Fatal("RegisterProvider accepted batch with invalid rule")
	}

	if _, ok := broker.Definition("broken_tool", tools.Version); ok {
		t.Fatal("broken_tool definition was committed despite batch failure")
	}

	decision, err := broker.Evaluate(t.Context(), &ToolRequest{
		RequestID:   "req-1",
		ToolName:    tools.KBSearchToolName,
		ToolVersion: tools.Version,
		Arguments:   json.RawMessage(`{"query":"refunds"}`),
		Trust:       approval.TrustOwnerInput,
	})
	if err != nil {
		t.Fatalf("Evaluate pre-existing kb_search rule: %v", err)
	}
	if decision.Outcome != approval.OutcomeAllow {
		t.Fatalf("pre-existing kb_search rule mutated: outcome = %q, want %q", decision.Outcome, approval.OutcomeAllow)
	}

	if _, err := broker.Execute(t.Context(), &ToolRequest{
		RequestID:   "req-2",
		ToolName:    tools.KBSearchToolName,
		ToolVersion: tools.Version,
		Arguments:   json.RawMessage(`{"query":"refunds"}`),
		Trust:       approval.TrustOwnerInput,
	}); !fault.HasCode(err, ResultCapabilityUnavailable) {
		t.Fatalf("kb_search adapter was committed despite batch failure: err = %v", err)
	}
}

func TestBrokerRegisterProviderRegistersAndExecutesTools(t *testing.T) {
	broker, err := New(&Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	reg := sampleRegistration("custom_search")
	if err := broker.RegisterProvider(stubToolProvider{regs: []ToolRegistration{reg}}); err != nil {
		t.Fatalf("RegisterProvider: %v", err)
	}
	def, ok := broker.Definition("custom_search", tools.Version)
	if !ok || def.Name != "custom_search" {
		t.Fatalf("Definition = %+v, %v", def, ok)
	}
	res, err := broker.Execute(t.Context(), &ToolRequest{
		RequestID:   "req-1",
		ToolName:    "custom_search",
		ToolVersion: tools.Version,
		Arguments:   json.RawMessage(`{"q":"hello"}`),
		Trust:       approval.TrustOwnerInput,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Class != ResultOK || string(res.Output) != `{"ok":true}` {
		t.Fatalf("result = %+v", res)
	}
}

func TestDefaultRuleMatchesStandardConstraintsAndClonesCapabilities(t *testing.T) {
	if got := DefaultRule(nil); got.ToolName != "" {
		t.Fatalf("DefaultRule(nil) = %+v, want zero rule", got)
	}

	caps := []string{"public-web"}
	def := &tools.Definition{
		Name:                 "custom_fetch",
		Version:              tools.Version,
		RequiredCapabilities: caps,
		RequiresApproval:     true,
	}
	rule := DefaultRule(def)
	caps[0] = "mutated"

	if rule.ToolName != "custom_fetch" || rule.ToolVersion != tools.Version || !rule.RequiresApproval {
		t.Fatalf("DefaultRule = %+v", rule)
	}
	if !slices.Equal(rule.RequiredCapabilities, []string{"public-web"}) {
		t.Fatalf("RequiredCapabilities = %v, want [public-web]", rule.RequiredCapabilities)
	}
	wantTrust := []approval.TrustLabel{
		approval.TrustOwnerInput,
		approval.TrustTrustedConfiguration,
		approval.TrustDerivedUntrusted,
	}
	if !slices.Equal(rule.AllowedTrust, wantTrust) {
		t.Fatalf("AllowedTrust = %v, want %v", rule.AllowedTrust, wantTrust)
	}
	if rule.Constraints.MaxOutputBytes != 64*1024 || rule.Constraints.Timeout != 30*time.Second || rule.Constraints.AllowNetwork {
		t.Fatalf("Constraints = %+v", rule.Constraints)
	}
}

func TestBrokerRegisterProviderInstallsArgumentCheckAndApprovalGuardWithNilOptions(t *testing.T) {
	var prompts int
	broker, err := New(&Options{
		ApprovalDecider: func(context.Context, *ApprovalPrompt) (bool, error) {
			prompts++
			return false, nil
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	reg := sampleRegistration("guarded_tool")
	reg.Rule = DefaultRule(&reg.Definition)
	reg.ArgumentCheck = func(raw json.RawMessage) error {
		var payload struct {
			Q string `json:"q"`
		}
		if err := json.Unmarshal(raw, &payload); err != nil {
			return err
		}
		if payload.Q == "reject" {
			return errors.New("rejected argument")
		}
		return nil
	}
	reg.ApprovalGuard = func(context.Context, *ToolRequest) (bool, error) {
		return true, nil
	}
	if err := broker.RegisterProvider(stubToolProvider{regs: []ToolRegistration{reg}}); err != nil {
		t.Fatalf("RegisterProvider: %v", err)
	}

	_, err = broker.Execute(t.Context(), &ToolRequest{
		RequestID:   "req-bad",
		ToolName:    "guarded_tool",
		ToolVersion: tools.Version,
		Arguments:   json.RawMessage(`{"q":"reject"}`),
		Trust:       approval.TrustOwnerInput,
	})
	if !fault.HasCode(err, ResultInvalidArgument) {
		t.Fatalf("bad argument err = %v, want %q", err, ResultInvalidArgument)
	}
	if prompts != 0 {
		t.Fatalf("prompts = %d, want 0 when argument check fails", prompts)
	}

	_, err = broker.Execute(t.Context(), &ToolRequest{
		RequestID:   "req-guarded",
		ToolName:    "guarded_tool",
		ToolVersion: tools.Version,
		Arguments:   json.RawMessage(`{"q":"allow"}`),
		Trust:       approval.TrustOwnerInput,
	})
	if !fault.HasCode(err, ResultApprovalRequired) {
		t.Fatalf("guarded call err = %v, want %q", err, ResultApprovalRequired)
	}
	if prompts != 1 {
		t.Fatalf("prompts = %d, want 1", prompts)
	}

	cleared := sampleRegistration("guarded_tool")
	cleared.Rule = DefaultRule(&cleared.Definition)
	if err := broker.RegisterProvider(stubToolProvider{regs: []ToolRegistration{cleared}}); err != nil {
		t.Fatalf("re-RegisterProvider without hooks: %v", err)
	}
	res, err := broker.Execute(t.Context(), &ToolRequest{
		RequestID:   "req-cleared",
		ToolName:    "guarded_tool",
		ToolVersion: tools.Version,
		Arguments:   json.RawMessage(`{"q":"reject"}`),
		Trust:       approval.TrustOwnerInput,
	})
	if err != nil {
		t.Fatalf("Execute after clearing hooks: %v", err)
	}
	if res.Class != ResultOK || prompts != 1 {
		t.Fatalf("result = %+v, prompts = %d, want ok and unchanged prompts", res, prompts)
	}
}

func TestBrokerRegisterProviderAtomicFailurePreservesHooks(t *testing.T) {
	var prompts int
	broker, err := New(&Options{
		ApprovalDecider: func(context.Context, *ApprovalPrompt) (bool, error) {
			prompts++
			return false, nil
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	initial := sampleRegistration("atomic_tool")
	initial.Rule = DefaultRule(&initial.Definition)
	initial.Adapter = func(context.Context, *ToolRequest, approval.Constraints) (ToolResult, error) {
		return ToolResult{Class: ResultOK, Output: json.RawMessage(`{"version":"initial"}`)}, nil
	}
	initial.ArgumentCheck = func(raw json.RawMessage) error {
		var payload struct {
			Q string `json:"q"`
		}
		if err := json.Unmarshal(raw, &payload); err != nil {
			return err
		}
		if payload.Q == "bad" {
			return errors.New("initial check rejected")
		}
		return nil
	}
	initial.ApprovalGuard = func(context.Context, *ToolRequest) (bool, error) {
		return false, nil
	}
	if err := broker.RegisterProvider(stubToolProvider{regs: []ToolRegistration{initial}}); err != nil {
		t.Fatalf("initial RegisterProvider: %v", err)
	}

	replacement := sampleRegistration("atomic_tool")
	replacement.Rule = DefaultRule(&replacement.Definition)
	replacement.Rule.RequiresApproval = true
	replacement.Adapter = func(context.Context, *ToolRequest, approval.Constraints) (ToolResult, error) {
		return ToolResult{Class: ResultOK, Output: json.RawMessage(`{"version":"mutated"}`)}, nil
	}
	replacement.ArgumentCheck = func(json.RawMessage) error {
		return errors.New("replacement check rejected everything")
	}
	replacement.ApprovalGuard = func(context.Context, *ToolRequest) (bool, error) {
		return true, nil
	}

	broken := sampleRegistration("broken_sibling")
	broken.Rule = DefaultRule(&broken.Definition)
	broken.Rule.Constraints.MaxOutputBytes = -1

	if err := broker.RegisterProvider(stubToolProvider{regs: []ToolRegistration{replacement, broken}}); err == nil {
		t.Fatal("RegisterProvider accepted batch with invalid rule")
	}

	if _, ok := broker.Definition("broken_sibling", tools.Version); ok {
		t.Fatal("broken_sibling definition was committed despite batch failure")
	}
	if _, ok := broker.Policy().Rules["broken_sibling"]; ok {
		t.Fatal("broken_sibling rule was committed despite batch failure")
	}

	res, err := broker.Execute(t.Context(), &ToolRequest{
		RequestID:   "req-ok",
		ToolName:    "atomic_tool",
		ToolVersion: tools.Version,
		Arguments:   json.RawMessage(`{"q":"ok"}`),
		Trust:       approval.TrustOwnerInput,
	})
	if err != nil {
		t.Fatalf("Execute with original valid argument failed after rejected batch: %v", err)
	}
	if string(res.Output) != `{"version":"initial"}` {
		t.Fatalf("adapter output = %s, want initial adapter output", res.Output)
	}
	if prompts != 0 {
		t.Fatalf("prompts = %d, want 0 (original guard and rule must remain intact)", prompts)
	}

	_, err = broker.Execute(t.Context(), &ToolRequest{
		RequestID:   "req-bad",
		ToolName:    "atomic_tool",
		ToolVersion: tools.Version,
		Arguments:   json.RawMessage(`{"q":"bad"}`),
		Trust:       approval.TrustOwnerInput,
	})
	if !fault.HasCode(err, ResultInvalidArgument) {
		t.Fatalf("original ArgumentCheck did not run after rejected batch: err = %v", err)
	}
}

func TestBrokerUnregisterToolClearsArgumentCheckAndApprovalGuard(t *testing.T) {
	var prompts int
	broker, err := New(&Options{
		ApprovalDecider: func(context.Context, *ApprovalPrompt) (bool, error) {
			prompts++
			return false, nil
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	reg := sampleRegistration("ephemeral_tool")
	reg.Rule = DefaultRule(&reg.Definition)
	reg.ArgumentCheck = func(raw json.RawMessage) error {
		var payload struct {
			Q string `json:"q"`
		}
		if err := json.Unmarshal(raw, &payload); err != nil {
			return err
		}
		if payload.Q == "blocked" {
			return errors.New("blocked by stale argument check")
		}
		return nil
	}
	reg.ApprovalGuard = func(context.Context, *ToolRequest) (bool, error) {
		return true, nil
	}
	if err := broker.RegisterProvider(stubToolProvider{regs: []ToolRegistration{reg}}); err != nil {
		t.Fatalf("RegisterProvider: %v", err)
	}

	if _, err := broker.Execute(t.Context(), &ToolRequest{
		RequestID:   "req-1",
		ToolName:    "ephemeral_tool",
		ToolVersion: tools.Version,
		Arguments:   json.RawMessage(`{"q":"blocked"}`),
		Trust:       approval.TrustOwnerInput,
	}); !fault.HasCode(err, ResultInvalidArgument) {
		t.Fatalf("pre-unregister bad arg err = %v, want %q", err, ResultInvalidArgument)
	}
	if _, err := broker.Execute(t.Context(), &ToolRequest{
		RequestID:   "req-2",
		ToolName:    "ephemeral_tool",
		ToolVersion: tools.Version,
		Arguments:   json.RawMessage(`{"q":"ok"}`),
		Trust:       approval.TrustOwnerInput,
	}); !fault.HasCode(err, ResultApprovalRequired) || prompts != 1 {
		t.Fatalf("pre-unregister guarded call err = %v, prompts = %d", err, prompts)
	}

	broker.UnregisterTool("ephemeral_tool", tools.Version)

	bareDef := reg.Definition
	bareRule := DefaultRule(&bareDef)
	if err := broker.RegisterTool(&bareDef, reg.Adapter, &bareRule); err != nil {
		t.Fatalf("RegisterTool after unregister: %v", err)
	}

	res, err := broker.Execute(t.Context(), &ToolRequest{
		RequestID:   "req-3",
		ToolName:    "ephemeral_tool",
		ToolVersion: tools.Version,
		Arguments:   json.RawMessage(`{"q":"blocked"}`),
		Trust:       approval.TrustOwnerInput,
	})
	if err != nil {
		t.Fatalf("Execute after unregister + re-register: %v", err)
	}
	if res.Class != ResultOK || prompts != 1 {
		t.Fatalf("result = %+v, prompts = %d, want ok without stale hooks firing", res, prompts)
	}
}

func TestBrokerConcurrentProviderMutationAndExecution(t *testing.T) {
	broker, err := New(&Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	steady := sampleRegistration("steady_tool")
	steady.Rule = DefaultRule(&steady.Definition)
	steady.ArgumentCheck = func(json.RawMessage) error { return nil }
	steady.ApprovalGuard = func(context.Context, *ToolRequest) (bool, error) { return false, nil }
	if err := broker.RegisterProvider(stubToolProvider{regs: []ToolRegistration{steady}}); err != nil {
		t.Fatalf("RegisterProvider steady: %v", err)
	}

	churn := sampleRegistration("churn_tool")
	churn.Rule = DefaultRule(&churn.Definition)
	churn.ArgumentCheck = func(json.RawMessage) error { return nil }
	churn.ApprovalGuard = func(context.Context, *ToolRequest) (bool, error) { return false, nil }

	const iterations = 100
	var execErr atomic.Pointer[error]
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		for range iterations {
			if err := broker.RegisterProvider(stubToolProvider{regs: []ToolRegistration{churn}}); err != nil {
				execErr.CompareAndSwap(nil, &err)
				return
			}
			broker.UnregisterTool("churn_tool", tools.Version)
		}
	}()

	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx := t.Context()
			for range iterations {
				req := &ToolRequest{
					RequestID:   "req-steady",
					ToolName:    "steady_tool",
					ToolVersion: tools.Version,
					Arguments:   json.RawMessage(`{"q":"ok"}`),
					Trust:       approval.TrustOwnerInput,
				}
				if _, err := broker.Evaluate(ctx, req); err != nil {
					execErr.CompareAndSwap(nil, &err)
					return
				}
				if _, err := broker.Execute(ctx, req); err != nil {
					execErr.CompareAndSwap(nil, &err)
					return
				}
				_ = broker.Policy()
			}
		}()
	}

	wg.Wait()
	if errPtr := execErr.Load(); errPtr != nil {
		t.Fatalf("concurrent broker error: %v", *errPtr)
	}
}

func TestBrokerPolicyReturnsIsolatedDeepCopy(t *testing.T) {
	broker, err := New(&Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	reg := sampleRegistration("policy_snapshot_tool")
	reg.Definition.RequiredCapabilities = []string{"public-web"}
	reg.Rule = DefaultRule(&reg.Definition)
	if err := broker.RegisterProvider(stubToolProvider{regs: []ToolRegistration{reg}}); err != nil {
		t.Fatalf("RegisterProvider: %v", err)
	}

	snapshot := broker.Policy()
	if snapshot.Version != broker.PolicyVersion() {
		t.Fatalf("Policy().Version = %q, want %q", snapshot.Version, broker.PolicyVersion())
	}
	rule, ok := snapshot.Rules["policy_snapshot_tool"]
	if !ok {
		t.Fatal("registered provider rule missing from Broker.Policy()")
	}
	rule.RequiredCapabilities[0] = "tampered-cap"
	rule.AllowedTrust[0] = approval.TrustUntrustedExternal
	delete(snapshot.Rules, "policy_snapshot_tool")

	fresh := broker.Policy()
	got, ok := fresh.Rules["policy_snapshot_tool"]
	if !ok {
		t.Fatal("deleting from Broker.Policy() snapshot mutated broker engine")
	}
	if !slices.Equal(got.RequiredCapabilities, []string{"public-web"}) {
		t.Fatalf("RequiredCapabilities = %v, want [public-web]", got.RequiredCapabilities)
	}
	if got.AllowedTrust[0] != approval.TrustOwnerInput {
		t.Fatalf("AllowedTrust[0] = %q, want %q", got.AllowedTrust[0], approval.TrustOwnerInput)
	}

	broker.UnregisterTool("policy_snapshot_tool", tools.Version)
	if _, exists := broker.Policy().Rules["policy_snapshot_tool"]; exists {
		t.Fatal("unregistered tool rule remained in Broker.Policy()")
	}
}
