package toolbrokertest

import (
	"encoding/json"
	"testing"

	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/toolbroker"
	"github.com/anggasct/aura/internal/platform/tools"
)

type Stub struct {
	Name             string
	RequiresApproval bool
	Run              toolbroker.Adapter
}

func Register(tb testing.TB, broker *toolbroker.Broker, stub Stub) {
	tb.Helper()
	definition := &tools.Definition{
		Name:      stub.Name,
		Version:   "v1",
		Validator: func(raw json.RawMessage) (json.RawMessage, error) { return raw, nil },
	}
	rule := &approval.Rule{ToolName: stub.Name, ToolVersion: "v1", RequiresApproval: stub.RequiresApproval}
	if err := broker.RegisterTool(definition, stub.Run, rule); err != nil {
		tb.Fatalf("register stub tool %s: %v", stub.Name, err)
	}
}
