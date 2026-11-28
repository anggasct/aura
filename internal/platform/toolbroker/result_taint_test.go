package toolbroker

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"

	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/capability/capabilitytest"
	"github.com/anggasct/aura/internal/platform/tools"
)

func TestResultTaintFollowsTheDefinition(t *testing.T) {
	t.Run("registration rejects conflicting or external OutputTaint", func(t *testing.T) {
		broker, err := New(&Options{HostCapabilities: capabilitytest.Host(t)})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		noop := func(context.Context, *ToolRequest, approval.Constraints) (ToolResult, error) {
			return ToolResult{Output: json.RawMessage(`{}`)}, nil
		}
		conflict := tools.Definition{
			Name: "bad_conflict", Version: tools.Version, UntrustedOutput: true,
			OutputTaint: func(json.RawMessage) bool { return true },
			Validator:   func(raw json.RawMessage) (json.RawMessage, error) { return raw, nil },
		}
		conflictRule := DefaultRule(&conflict)
		if err := broker.RegisterTool(&conflict, noop, &conflictRule); err == nil {
			t.Error("registered a tool with both UntrustedOutput and OutputTaint")
		}
		serverDef := tools.Definition{
			Name: "mcp_srv_bad", Version: tools.Version, Server: "srv",
			OutputTaint: func(json.RawMessage) bool { return false },
			Validator:   func(raw json.RawMessage) (json.RawMessage, error) { return raw, nil },
		}
		serverRule := DefaultRule(&serverDef)
		if err := broker.RegisterTool(&serverDef, noop, &serverRule); err == nil {
			t.Error("registered a server tool with OutputTaint")
		}
	})

	broker, err := New(&Options{HostCapabilities: capabilitytest.Host(t)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	cleanDef := tools.Definition{
		Name: "clean_tool", Version: tools.Version,
		Validator: func(raw json.RawMessage) (json.RawMessage, error) { return raw, nil },
	}
	alwaysDef := tools.Definition{
		Name: "always_tool", Version: tools.Version, UntrustedOutput: true,
		Validator: func(raw json.RawMessage) (json.RawMessage, error) { return raw, nil },
	}
	perResultDef := tools.Definition{
		Name: "per_result_tool", Version: tools.Version,
		OutputTaint: func(raw json.RawMessage) bool {
			var out struct {
				Trusted bool `json:"trusted"`
			}
			if err := json.Unmarshal(raw, &out); err != nil {
				return true
			}
			return !out.Trusted
		},
		Validator: func(raw json.RawMessage) (json.RawMessage, error) { return raw, nil },
	}
	cleanRule := DefaultRule(&cleanDef)
	if err := broker.RegisterTool(&cleanDef, func(context.Context, *ToolRequest, approval.Constraints) (ToolResult, error) {
		return ToolResult{Output: json.RawMessage(`{"ok":true}`)}, nil
	}, &cleanRule); err != nil {
		t.Fatalf("register clean: %v", err)
	}
	alwaysRule := DefaultRule(&alwaysDef)
	if err := broker.RegisterTool(&alwaysDef, func(context.Context, *ToolRequest, approval.Constraints) (ToolResult, error) {
		return ToolResult{Output: json.RawMessage(`{"ok":true}`)}, nil
	}, &alwaysRule); err != nil {
		t.Fatalf("register always: %v", err)
	}
	var nextPayload json.RawMessage
	perResultRule := DefaultRule(&perResultDef)
	if err := broker.RegisterTool(&perResultDef, func(context.Context, *ToolRequest, approval.Constraints) (ToolResult, error) {
		return ToolResult{Output: nextPayload}, nil
	}, &perResultRule); err != nil {
		t.Fatalf("register per_result: %v", err)
	}

	res, err := broker.Execute(t.Context(), brokerRequest("clean_tool", `{}`))
	if err != nil || res.Untrusted {
		t.Fatalf("clean tool = %+v %v, want Untrusted=false", res, err)
	}
	res, err = broker.Execute(t.Context(), brokerRequest("always_tool", `{}`))
	if err != nil || !res.Untrusted {
		t.Fatalf("always tool = %+v %v, want Untrusted=true", res, err)
	}

	nextPayload = json.RawMessage(`{"trusted":true}`)
	res, err = broker.Execute(t.Context(), brokerRequest("per_result_tool", `{}`))
	if err != nil || res.Untrusted {
		t.Fatalf("trusted per_result = %+v %v, want Untrusted=false", res, err)
	}
	nextPayload = json.RawMessage(`{"trusted":false}`)
	res, err = broker.Execute(t.Context(), brokerRequest("per_result_tool", `{}`))
	if err != nil || !res.Untrusted {
		t.Fatalf("untrusted per_result = %+v %v, want Untrusted=true", res, err)
	}
	nextPayload = json.RawMessage(`"not-an-object"`)
	res, err = broker.Execute(t.Context(), brokerRequest("per_result_tool", `{}`))
	if err != nil || !res.Untrusted {
		t.Fatalf("unparsable per_result = %+v %v, want fail-closed Untrusted=true", res, err)
	}
}

func TestServerAndPluginResultsAlwaysTaint(t *testing.T) {
	broker, err := New(&Options{HostCapabilities: capabilitytest.Host(t)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	noop := func(context.Context, *ToolRequest, approval.Constraints) (ToolResult, error) {
		return ToolResult{Output: json.RawMessage(`{"ok":true}`)}, nil
	}
	mcpDef := tools.Definition{
		Name: "mcp_docs_search", Version: tools.Version, Server: "docs",
		Validator: func(raw json.RawMessage) (json.RawMessage, error) { return raw, nil },
	}
	pluginDef := tools.Definition{
		Name: "plugin_mail_list", Version: tools.Version, Plugin: &tools.PluginRef{ID: "mail", Description: "mail"},
		Validator: func(raw json.RawMessage) (json.RawMessage, error) { return raw, nil },
	}
	mcpRule := DefaultRule(&mcpDef)
	if err := broker.RegisterTool(&mcpDef, noop, &mcpRule); err != nil {
		t.Fatalf("register mcp: %v", err)
	}
	pluginRule := DefaultRule(&pluginDef)
	if err := broker.RegisterTool(&pluginDef, noop, &pluginRule); err != nil {
		t.Fatalf("register plugin: %v", err)
	}

	mcpRes, err := broker.Execute(t.Context(), brokerRequest("mcp_docs_search", `{}`))
	if err != nil || !mcpRes.Untrusted {
		t.Fatalf("mcp result = %+v %v, want Untrusted=true", mcpRes, err)
	}
	pluginRes, err := broker.Execute(t.Context(), brokerRequest("plugin_mail_list", `{}`))
	if err != nil || !pluginRes.Untrusted {
		t.Fatalf("plugin result = %+v %v, want Untrusted=true", pluginRes, err)
	}
}

func TestReplayedResultTaintEqualsTheLiveOne(t *testing.T) {
	var runs atomic.Int32
	broker, err := New(&Options{
		HostCapabilities:  capabilitytest.Host(t),
		Effects:           newBrokerEffectExecutor(t),
		ApprovalReachable: func(string) bool { return true },
		ApprovalDecider:   func(context.Context, *ApprovalPrompt) (bool, error) { return true, nil },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	def := tools.Definition{
		Name: "effect_per_result", Version: tools.Version,
		Effectful: true,
		OutputTaint: func(raw json.RawMessage) bool {
			var out struct {
				Clean bool `json:"clean"`
			}
			if err := json.Unmarshal(raw, &out); err != nil {
				return true
			}
			return !out.Clean
		},
		Validator: func(raw json.RawMessage) (json.RawMessage, error) { return raw, nil },
	}
	rule := DefaultRule(&def)
	if err := broker.RegisterTool(&def, func(context.Context, *ToolRequest, approval.Constraints) (ToolResult, error) {
		runs.Add(1)
		return ToolResult{Output: json.RawMessage(`{"clean":false}`)}, nil
	}, &rule); err != nil {
		t.Fatalf("RegisterTool: %v", err)
	}

	req := brokerRequest("effect_per_result", `{}`)
	req.IdempotencyKey = "replay-taint-key"
	req.EventSequence = 1
	live, err := broker.Execute(t.Context(), req)
	if err != nil {
		t.Fatalf("live Execute: %v", err)
	}
	replayed, err := broker.Execute(t.Context(), req)
	if err != nil {
		t.Fatalf("replay Execute: %v", err)
	}
	if runs.Load() != 1 {
		t.Fatalf("adapter runs = %d, want 1", runs.Load())
	}
	if !live.Untrusted || replayed.Untrusted != live.Untrusted {
		t.Fatalf("live.Untrusted=%v replayed.Untrusted=%v, want both true", live.Untrusted, replayed.Untrusted)
	}
}
