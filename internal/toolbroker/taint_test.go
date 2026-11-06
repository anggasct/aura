package toolbroker

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/approval"
	"github.com/anggasct/aura/internal/capability/capabilitytest"
	"github.com/anggasct/aura/internal/durable"
	"github.com/anggasct/aura/internal/principal"
	"github.com/anggasct/aura/internal/tools"
)

type taintHarness struct {
	broker  *Broker
	calls   map[string]int
	prompts int
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
		ApprovalDecider: func(context.Context, *ApprovalPrompt) (bool, error) {
			harness.prompts++
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
			if classOf(err) != ResultKnowledgeSinkNoApprover {
				t.Fatalf("error = %v, want knowledge_sink_no_approver", err)
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
	if _, err := harness.broker.Execute(t.Context(), request); classOf(err) != ResultKnowledgeSinkNoApprover {
		t.Fatalf("unreachable session error = %v", err)
	}
	if len(asked) != 2 || asked[0] != "session-1" || asked[1] != "session-2" {
		t.Errorf("reachability asked for %v", asked)
	}
}

func TestTaintedApprovalOnADurableTurnIsDeniedWithoutParking(t *testing.T) {
	harness := newTaintBroker(t, always, nil)
	backend := durable.NewFake()
	invocations := make(chan durable.Invocation, 1)
	backend.RegisterHandler("driver", func(_ context.Context, inv durable.Invocation) error {
		invocations <- inv
		select {}
	})
	if _, err := backend.Start(t.Context(), durable.StartRequest{Handler: "driver", Key: "turn-1"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	var inv durable.Invocation
	select {
	case inv = <-invocations:
	case <-time.After(5 * time.Second):
		t.Fatal("driver invocation never started")
	}
	sink := &recordingSink{ready: make(chan struct{})}
	ctx := durable.WithTurnScope(WithApprovalSink(t.Context(), sink), durable.NewTurnScope(inv))
	call := sinkCalls["web_fetch"]
	request := classifiedRequest("web_fetch", call.arguments, principal.ClassOwner, call.capabilities...)
	request.Tainted = true

	done := make(chan error, 1)
	go func() {
		_, err := harness.broker.Execute(ctx, request)
		done <- err
	}()
	select {
	case err := <-done:
		if classOf(err) != ResultKnowledgeSinkNoApprover {
			t.Fatalf("error = %v, want knowledge_sink_no_approver", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the tainted durable call parked instead of failing at once")
	}
	if len(sink.snapshot()) != 0 || harness.prompts != 0 || harness.calls["web_fetch"] != 0 {
		t.Errorf("approval events = %d prompts = %d calls = %d, want none", len(sink.snapshot()), harness.prompts, harness.calls["web_fetch"])
	}
}

func TestKnowledgeSinkNoApproverIsAStableClass(t *testing.T) {
	if !ResultKnowledgeSinkNoApprover.stable() {
		t.Fatal("knowledge_sink_no_approver is not listed in stable()")
	}
	if string(ResultKnowledgeSinkNoApprover) != "knowledge_sink_no_approver" {
		t.Errorf("class = %q", ResultKnowledgeSinkNoApprover)
	}
}
