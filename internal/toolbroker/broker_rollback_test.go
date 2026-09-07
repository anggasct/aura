package toolbroker

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/anggasct/aura/internal/approval"
	"github.com/anggasct/aura/internal/tools"
)

func TestBrokerRegisterRuleFailureLeavesNoResidue(t *testing.T) {
	broker, err := New(&Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	toolName := "half_registered"
	def := tools.Definition{
		Name:    toolName,
		Version: "v1",
		Validator: func(raw json.RawMessage) (json.RawMessage, error) {
			return raw, nil
		},
	}
	adapter := func(_ context.Context, req *ToolRequest, _ approval.Constraints) (ToolResult, error) {
		return ToolResult{Output: req.Arguments}, nil
	}
	rule := approval.Rule{
		ToolName:     toolName,
		ToolVersion:  "v1",
		AllowedTrust: []approval.TrustLabel{"bogus-label"},
	}
	if err := broker.RegisterTool(&def, adapter, &rule); err == nil {
		t.Fatal("expected RegisterTool to fail for invalid rule")
	}
	for _, d := range broker.Definitions() {
		if d.Name == toolName {
			t.Fatalf("tool %q must not remain after failed registration", toolName)
		}
	}
	_, err = broker.Execute(t.Context(), brokerRequest(toolName, `{}`, "public-web"))
	if err == nil {
		t.Fatal("expected execute to fail for unregistered tool")
	}
}
