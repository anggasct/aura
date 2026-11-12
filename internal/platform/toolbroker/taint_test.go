package toolbroker

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/capability/capabilitytest"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/tools"
)

type taintHarness struct {
	broker  *Broker
	calls   map[string]int
	prompts int
	reasons []string
	seq     int
}

func newTaintBroker(t *testing.T, reachable func(sessionID string) bool, access AccessPolicy) *taintHarness {
	t.Helper()
	harness := &taintHarness{calls: map[string]int{}}
	adapter := func(name string) Adapter {
		return func(context.Context, *ToolRequest, approval.Constraints) (ToolResult, error) {
			harness.calls[name]++
			return ToolResult{Output: json.RawMessage(`{"ok":true}`)}, nil
		}
	}
	adapters := map[string]Adapter{}
	for _, name := range []string{"read_file", "list_dir", "web_fetch", "web_search", "workflow_start", "workflow_propose", "exec", "write_file"} {
		adapters[name+"@v1"] = adapter(name)
	}
	broker, err := New(&Options{
		HostCapabilities: capabilitytest.Host(t), Effects: newBrokerEffectExecutor(t),
		Adapters: adapters,
		ApprovalDecider: func(_ context.Context, prompt *ApprovalPrompt) (bool, error) {
			harness.prompts++
			harness.reasons = append(harness.reasons, prompt.ReasonCode)
			return true, nil
		},
		Access:            access,
		ApprovalReachable: reachable,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	harness.broker = broker
	return harness
}

func always(string) bool { return true }

func (h *taintHarness) call(name, arguments string, tainted bool, class principal.Class, capabilities ...string) error {
	h.seq++
	request := classifiedRequest(name, arguments, class, capabilities...)
	request.Tainted = tainted
	request.RequestID = fmt.Sprintf("request-%d", h.seq)
	request.IdempotencyKey = fmt.Sprintf("idempotency-%d", h.seq)
	request.EventSequence = uint64(h.seq)
	_, err := h.broker.Execute(context.Background(), request)
	return err
}

var sinkCalls = map[string]struct {
	arguments    string
	capabilities []string
}{
	"web_fetch":        {`{"url":"https://example.com/x"}`, []string{"public-web"}},
	"web_search":       {`{"query":"x"}`, []string{"provider-search"}},
	"workflow_start":   {`{"id":"refund"}`, nil},
	"workflow_propose": {`{"spec":{"goal":"g","steps":[{"id":"a"}]}}`, nil},
}

func TestTaintedTurnsNeedApprovalForEverySink(t *testing.T) {
	for name, call := range sinkCalls {
		t.Run(name, func(t *testing.T) {
			harness := newTaintBroker(t, always, nil)
			if err := harness.call(name, call.arguments, false, principal.ClassOwner, call.capabilities...); err != nil {
				t.Fatalf("untainted %s: %v", name, err)
			}
			if harness.prompts != 0 {
				t.Fatalf("untainted %s prompted %d times, want 0", name, harness.prompts)
			}
			if err := harness.call(name, call.arguments, true, principal.ClassOwner, call.capabilities...); err != nil {
				t.Fatalf("tainted %s with an approver: %v", name, err)
			}
			if harness.prompts != 1 || harness.calls[name] != 2 {
				t.Errorf("prompts = %d calls = %d, want 1 prompt and 2 executions", harness.prompts, harness.calls[name])
			}
		})
	}
}

func TestTaintedTurnsKeepReadToolsUnprompted(t *testing.T) {
	harness := newTaintBroker(t, nil, nil)
	if err := harness.call("read_file", `{"path":"notes.txt"}`, true, principal.ClassOwner, "workspace-read"); err != nil {
		t.Fatalf("tainted read_file: %v", err)
	}
	if err := harness.call("list_dir", `{"path":"."}`, true, principal.ClassOwner, "workspace-read"); err != nil {
		t.Fatalf("tainted list_dir: %v", err)
	}
	if harness.prompts != 0 || harness.calls["read_file"] != 1 || harness.calls["list_dir"] != 1 {
		t.Errorf("prompts = %d calls = %v, want both read tools to run without a prompt even with no approver", harness.prompts, harness.calls)
	}
}

func TestTaintedMCPAndPluginToolsCountAsSinks(t *testing.T) {
	tests := map[string]*tools.Definition{
		"mcp":    {Name: "mcp_docs_search", Version: "v1", Server: "docs"},
		"plugin": {Name: "plugin_weather_forecast", Version: "v1", Plugin: &tools.PluginRef{ID: "weather", Description: "forecast"}},
	}
	for name, definition := range tests {
		t.Run(name, func(t *testing.T) {
			harness := newTaintBroker(t, always, nil)
			definition.Schema = json.RawMessage(`{"type":"object"}`)
			definition.Validator = func(raw json.RawMessage) (json.RawMessage, error) { return raw, nil }
			rule := approval.Rule{
				ToolName: definition.Name, ToolVersion: "v1",
				AllowedTrust: []approval.TrustLabel{approval.TrustOwnerInput, approval.TrustDerivedUntrusted},
			}
			adapter := func(context.Context, *ToolRequest, approval.Constraints) (ToolResult, error) {
				harness.calls[definition.Name]++
				return ToolResult{Output: json.RawMessage(`{"ok":true}`)}, nil
			}
			if err := harness.broker.RegisterTool(definition, adapter, &rule); err != nil {
				t.Fatalf("RegisterTool: %v", err)
			}
			if err := harness.call(definition.Name, `{}`, false, principal.ClassOwner); err != nil || harness.prompts != 0 {
				t.Fatalf("untainted: err = %v prompts = %d", err, harness.prompts)
			}
			if err := harness.call(definition.Name, `{}`, true, principal.ClassOwner); err != nil || harness.prompts != 1 {
				t.Fatalf("tainted: err = %v prompts = %d, want one prompt", err, harness.prompts)
			}
		})
	}
}

func TestTaintedRequestsIgnoreStandingGrants(t *testing.T) {
	access := teamAccess()
	access.ceilings[principal.ClassTrusted] = []string{"exec", "read_file", "web_fetch", "write_file"}
	access.standing = true
	harness := newTaintBroker(t, always, access)

	if err := harness.call("exec", `{"command":["true"]}`, false, principal.ClassTrusted, "exec-linux"); err != nil {
		t.Fatalf("untainted exec: %v", err)
	}
	if harness.prompts != 0 || access.lookups != 1 {
		t.Fatalf("untainted: prompts = %d lookups = %d, want the standing grant to run it unprompted", harness.prompts, access.lookups)
	}
	for name, call := range map[string]struct {
		arguments    string
		capabilities []string
	}{
		"exec":      {`{"command":["true"]}`, []string{"exec-linux"}},
		"web_fetch": sinkCalls["web_fetch"],
	} {
		before := access.lookups
		promptsBefore := harness.prompts
		if err := harness.call(name, call.arguments, true, principal.ClassTrusted, call.capabilities...); err != nil {
			t.Fatalf("tainted %s: %v", name, err)
		}
		if access.lookups != before {
			t.Errorf("tainted %s consulted standing grants", name)
		}
		if harness.prompts != promptsBefore+1 {
			t.Errorf("tainted %s prompts = %d, want one more than %d", name, harness.prompts, promptsBefore)
		}
	}
}

func TestTaintedApprovalWithNoReachableApproverIsDeniedAtOnce(t *testing.T) {
	tests := map[string]func(string) bool{
		"nil reachability":    nil,
		"session unreachable": func(string) bool { return false },
	}
	for name, reachable := range tests {
		t.Run(name, func(t *testing.T) {
			harness := newTaintBroker(t, reachable, nil)
			call := sinkCalls["web_fetch"]
			err := harness.call("web_fetch", call.arguments, true, principal.ClassOwner, call.capabilities...)
			if classOf(err) != ResultUntrustedInputNoApprover {
				t.Fatalf("error = %v, want untrusted_input_no_approver", err)
			}
			if harness.prompts != 0 || harness.calls["web_fetch"] != 0 {
				t.Errorf("prompts = %d calls = %d, want neither", harness.prompts, harness.calls["web_fetch"])
			}
			if err := harness.call("web_fetch", call.arguments, false, principal.ClassOwner, call.capabilities...); err != nil {
				t.Errorf("an untainted sink must be unaffected: %v", err)
			}
		})
	}
}

func TestTaintedApprovalChecksReachabilityPerSession(t *testing.T) {
	var asked []string
	harness := newTaintBroker(t, func(sessionID string) bool {
		asked = append(asked, sessionID)
		return sessionID == "session-1"
	}, nil)
	call := sinkCalls["web_search"]
	if err := harness.call("web_search", call.arguments, true, principal.ClassOwner, call.capabilities...); err != nil {
		t.Fatalf("reachable session: %v", err)
	}
	request := classifiedRequest("web_search", call.arguments, principal.ClassOwner, call.capabilities...)
	request.Tainted, request.SessionID = true, "session-2"
	if _, err := harness.broker.Execute(t.Context(), request); classOf(err) != ResultUntrustedInputNoApprover {
		t.Fatalf("unreachable session error = %v", err)
	}
	if len(asked) != 2 || asked[0] != "session-1" || asked[1] != "session-2" {
		t.Errorf("reachability asked for %v", asked)
	}
}

func TestTaintedPublicTurnsNeverAskTheOwner(t *testing.T) {
	access := teamAccess()
	access.ceilings[principal.ClassPublic] = []string{"web_fetch"}
	access.ceilings[principal.ClassTrusted] = []string{"web_fetch"}
	harness := newTaintBroker(t, always, access)
	call := sinkCalls["web_fetch"]

	err := harness.call("web_fetch", call.arguments, true, principal.ClassPublic, call.capabilities...)
	if classOf(err) != ResultUntrustedInputNoApprover || !strings.Contains(err.Error(), "a public principal's turn never asks the owner") {
		t.Fatalf("tainted public web_fetch error = %v, want untrusted_input_no_approver for a public principal", err)
	}
	if harness.prompts != 0 || harness.calls["web_fetch"] != 0 {
		t.Fatalf("prompts = %d calls = %d, want neither", harness.prompts, harness.calls["web_fetch"])
	}
	if err := harness.call("web_fetch", call.arguments, false, principal.ClassPublic, call.capabilities...); err != nil {
		t.Fatalf("untainted public web_fetch: %v", err)
	}
	if harness.prompts != 0 || harness.calls["web_fetch"] != 1 {
		t.Fatalf("untainted: prompts = %d calls = %d, want the call to run unprompted", harness.prompts, harness.calls["web_fetch"])
	}
	if err := harness.call("web_fetch", call.arguments, true, principal.ClassTrusted, call.capabilities...); err != nil {
		t.Fatalf("tainted trusted web_fetch: %v", err)
	}
	if harness.prompts != 1 || harness.calls["web_fetch"] != 2 {
		t.Errorf("trusted: prompts = %d calls = %d, want one prompt and a run", harness.prompts, harness.calls["web_fetch"])
	}
}

func TestTaintedApprovalPromptCarriesTheUntrustedInputReason(t *testing.T) {
	access := teamAccess()
	harness := newTaintBroker(t, always, access)
	fetch := sinkCalls["web_fetch"]
	cases := []struct {
		tool      string
		arguments string
		class     principal.Class
		tainted   bool
		want      string
	}{
		{"web_fetch", fetch.arguments, principal.ClassOwner, true, approval.ReasonUntrustedInput},
		{"exec", `{"command":["true"]}`, principal.ClassTrusted, true, approval.ReasonUntrustedInput},
		{"exec", `{"command":["true"]}`, principal.ClassTrusted, false, string(approval.ErrorCodeApprovalRequired)},
	}
	for _, tc := range cases {
		harness.reasons = nil
		capabilities := []string{"exec-linux"}
		if tc.tool == "web_fetch" {
			capabilities = fetch.capabilities
		}
		if err := harness.call(tc.tool, tc.arguments, tc.tainted, tc.class, capabilities...); err != nil {
			t.Fatalf("%s tainted=%v: %v", tc.tool, tc.tainted, err)
		}
		if len(harness.reasons) != 1 || harness.reasons[0] != tc.want {
			t.Errorf("%s tainted=%v prompt reasons = %v, want [%s]", tc.tool, tc.tainted, harness.reasons, tc.want)
		}
	}
}

func TestUntrustedInputNoApproverIsAStableClass(t *testing.T) {
	if !ResultUntrustedInputNoApprover.stable() {
		t.Fatal("untrusted_input_no_approver is not listed in stable()")
	}
	if string(ResultUntrustedInputNoApprover) != "untrusted_input_no_approver" {
		t.Errorf("class = %q", ResultUntrustedInputNoApprover)
	}
}
