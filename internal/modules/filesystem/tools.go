package filesystem

import (
	"encoding/json"

	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/toolbroker"
	"github.com/anggasct/aura/internal/platform/tools"
)

const (
	ReadFileToolName  = "read_file"
	WriteFileToolName = "write_file"
	ListDirToolName   = "list_dir"
)

func ToolDefinitions() []tools.Definition {
	return []tools.Definition{
		{
			Name:                 ReadFileToolName,
			Version:              tools.Version,
			Schema:               []byte(`{"$schema":"https://json-schema.org/draft/2020-12/schema","$id":"aura://tools/read_file/v1","type":"object","properties":{"path":{"type":"string","minLength":1},"max_bytes":{"type":"integer","minimum":1}},"required":["path"],"additionalProperties":false}`),
			RequiredCapabilities: []string{"workspace-read"},
			Validator:            validateReadFile,
		},
		{
			Name:                 WriteFileToolName,
			Version:              tools.Version,
			Schema:               []byte(`{"$schema":"https://json-schema.org/draft/2020-12/schema","$id":"aura://tools/write_file/v1","type":"object","properties":{"path":{"type":"string","minLength":1},"content":{"type":"string"},"overwrite":{"type":"boolean"}},"required":["path","content"],"additionalProperties":false}`),
			RequiredCapabilities: []string{"workspace-write"},
			RequiresApproval:     true,
			Effectful:            true,
			Validator:            validateWriteFile,
		},
		{
			Name:                 ListDirToolName,
			Version:              tools.Version,
			Schema:               []byte(`{"$schema":"https://json-schema.org/draft/2020-12/schema","$id":"aura://tools/list_dir/v1","type":"object","properties":{"path":{"type":"string","minLength":1},"recursive":{"type":"boolean"}},"required":["path"],"additionalProperties":false}`),
			RequiredCapabilities: []string{"workspace-read"},
			Validator:            validateListDir,
		},
	}
}

func ToolRules() []approval.Rule {
	definitions := ToolDefinitions()
	rules := make([]approval.Rule, len(definitions))
	for i := range definitions {
		rules[i] = toolbroker.DefaultRule(&definitions[i])
	}
	return rules
}

func ToolRegistrations(adapters map[string]toolbroker.Adapter) []toolbroker.ToolRegistration {
	if len(adapters) == 0 {
		return nil
	}
	definitions := ToolDefinitions()
	registrations := make([]toolbroker.ToolRegistration, 0, len(definitions))
	for i := range definitions {
		definition := definitions[i]
		adapter, ok := adapters[definition.Key()]
		if !ok || adapter == nil {
			continue
		}
		registrations = append(registrations, toolbroker.ToolRegistration{
			Definition: definition,
			Adapter:    adapter,
			Rule:       toolbroker.DefaultRule(&definition),
		})
	}
	return registrations
}

type toolProvider struct {
	adapters map[string]toolbroker.Adapter
}

func NewToolProvider(adapters map[string]toolbroker.Adapter) toolbroker.ToolProvider {
	return &toolProvider{adapters: adapters}
}

func (p *toolProvider) Tools() []toolbroker.ToolRegistration {
	if p == nil {
		return nil
	}
	return ToolRegistrations(p.adapters)
}

func validateReadFile(raw json.RawMessage) (json.RawMessage, error) {
	value, err := tools.DecodeObject(raw)
	if err != nil {
		return nil, err
	}
	if err := tools.RejectUnknown(value, "path", "max_bytes"); err != nil {
		return nil, err
	}
	if err := tools.ValidateString(value, "path"); err != nil {
		return nil, err
	}
	if err := tools.ValidateOptionalInteger(value, "max_bytes", 1); err != nil {
		return nil, err
	}
	return json.Marshal(value)
}

func validateWriteFile(raw json.RawMessage) (json.RawMessage, error) {
	value, err := tools.DecodeObject(raw)
	if err != nil {
		return nil, err
	}
	if err := tools.RejectUnknown(value, "path", "content", "overwrite"); err != nil {
		return nil, err
	}
	if err := tools.ValidateString(value, "path"); err != nil {
		return nil, err
	}
	if err := tools.ValidateString(value, "content"); err != nil {
		return nil, err
	}
	if err := tools.ValidateOptionalBool(value, "overwrite"); err != nil {
		return nil, err
	}
	return json.Marshal(value)
}

func validateListDir(raw json.RawMessage) (json.RawMessage, error) {
	value, err := tools.DecodeObject(raw)
	if err != nil {
		return nil, err
	}
	if err := tools.RejectUnknown(value, "path", "recursive"); err != nil {
		return nil, err
	}
	if err := tools.ValidateString(value, "path"); err != nil {
		return nil, err
	}
	if err := tools.ValidateOptionalBool(value, "recursive"); err != nil {
		return nil, err
	}
	return json.Marshal(value)
}
