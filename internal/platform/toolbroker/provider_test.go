package toolbroker

import (
	"context"
	"encoding/json"
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
