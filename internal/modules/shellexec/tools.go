package shellexec

import (
	"encoding/json"

	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/toolbroker"
	"github.com/anggasct/aura/internal/platform/tools"
)

const ToolName = "exec"

func ToolDefinition() tools.Definition {
	return tools.Definition{
		Name:                 ToolName,
		Version:              tools.Version,
		Schema:               []byte(`{"$schema":"https://json-schema.org/draft/2020-12/schema","$id":"aura://tools/exec/v1","type":"object","properties":{"command":{"type":"array","items":{"type":"string"},"minItems":1},"shell":{"type":"boolean"}},"required":["command"],"additionalProperties":false}`),
		RequiredCapabilities: []string{"exec-linux"},
		RequiresApproval:     true,
		Effectful:            true,
		Validator:            validateArguments,
	}
}

func ToolRule() approval.Rule {
	definition := ToolDefinition()
	return toolbroker.DefaultRule(&definition)
}

func ToolRegistration(adapter toolbroker.Adapter) toolbroker.ToolRegistration {
	return toolbroker.ToolRegistration{
		Definition: ToolDefinition(),
		Adapter:    adapter,
		Rule:       ToolRule(),
	}
}

type toolProvider struct {
	adapter toolbroker.Adapter
}

func NewToolProvider(adapter toolbroker.Adapter) toolbroker.ToolProvider {
	return &toolProvider{adapter: adapter}
}

func (p *toolProvider) Tools() []toolbroker.ToolRegistration {
	if p == nil || p.adapter == nil {
		return nil
	}
	return []toolbroker.ToolRegistration{ToolRegistration(p.adapter)}
}

func validateArguments(raw json.RawMessage) (json.RawMessage, error) {
	value, err := tools.DecodeObject(raw)
	if err != nil {
		return nil, err
	}
	if err := tools.RejectUnknown(value, "command", "shell"); err != nil {
		return nil, err
	}
	if err := tools.ValidateStringArray(value, "command", true); err != nil {
		return nil, err
	}
	if err := tools.ValidateOptionalBool(value, "shell"); err != nil {
		return nil, err
	}
	return json.Marshal(value)
}
