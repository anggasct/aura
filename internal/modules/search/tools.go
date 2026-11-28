package search

import (
	"encoding/json"

	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/toolbroker"
	"github.com/anggasct/aura/internal/platform/tools"
)

const ToolName = "web_search"

func ToolDefinition() tools.Definition {
	return tools.Definition{
		Name:                 ToolName,
		Version:              tools.Version,
		Schema:               []byte(`{"$schema":"https://json-schema.org/draft/2020-12/schema","$id":"aura://tools/web_search/v1","type":"object","properties":{"query":{"type":"string","minLength":1},"max_results":{"type":"integer","minimum":1}},"required":["query"],"additionalProperties":false}`),
		RequiredCapabilities: []string{"provider-search"},
		ExternalSink:         true,
		SinkRelief:           &tools.SinkRelief{Kind: tools.SinkReliefBoundedQuery, Argument: "query", MaxBytes: MaxQueryBytes},
		Validator:            validateSearchArguments,
	}
}

func ToolRule() approval.Rule {
	def := ToolDefinition()
	return toolbroker.DefaultRule(&def)
}

func validateSearchArguments(raw json.RawMessage) (json.RawMessage, error) {
	value, err := tools.DecodeObject(raw)
	if err != nil {
		return nil, err
	}
	if err := tools.RejectUnknown(value, "query", "max_results"); err != nil {
		return nil, err
	}
	if err := tools.ValidateString(value, "query"); err != nil {
		return nil, err
	}
	if err := tools.ValidateOptionalInteger(value, "max_results", 1); err != nil {
		return nil, err
	}
	return json.Marshal(value)
}
