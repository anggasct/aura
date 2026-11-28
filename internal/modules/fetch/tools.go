package fetch

import (
	"encoding/json"

	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/toolbroker"
	"github.com/anggasct/aura/internal/platform/tools"
)

const ToolName = "web_fetch"

func ToolDefinition() tools.Definition {
	return tools.Definition{
		Name:                 ToolName,
		Version:              tools.Version,
		Schema:               []byte(`{"$schema":"https://json-schema.org/draft/2020-12/schema","$id":"aura://tools/web_fetch/v1","type":"object","properties":{"url":{"type":"string","format":"uri","minLength":1}},"required":["url"],"additionalProperties":false}`),
		RequiredCapabilities: []string{"public-web"},
		ExternalSink:         true,
		SinkRelief:           &tools.SinkRelief{Kind: tools.SinkReliefLinkProvenance, Argument: "url"},
		Validator:            validateFetchArguments,
	}
}

func ToolRule() approval.Rule {
	def := ToolDefinition()
	return toolbroker.DefaultRule(&def)
}

func validateFetchArguments(raw json.RawMessage) (json.RawMessage, error) {
	value, err := tools.DecodeObject(raw)
	if err != nil {
		return nil, err
	}
	if err := tools.RejectUnknown(value, "url"); err != nil {
		return nil, err
	}
	if err := tools.ValidateString(value, "url"); err != nil {
		return nil, err
	}
	return json.Marshal(value)
}
