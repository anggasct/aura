package toolsbuiltin

import (
	"encoding/json"

	"github.com/anggasct/aura/internal/platform/toolbroker"
	"github.com/anggasct/aura/internal/platform/tools"
)

type StepCatalog struct {
	definitions map[string]*tools.Definition
}

func NewStepCatalog(extra []tools.Definition) *StepCatalog {
	builtins := tools.Builtins()
	catalog := &StepCatalog{definitions: make(map[string]*tools.Definition, len(builtins)+len(extra))}
	for i := range builtins {
		catalog.definitions[builtins[i].Name] = &builtins[i]
	}
	for i := range extra {
		catalog.definitions[extra[i].Name] = &extra[i]
	}
	return catalog
}

func (c *StepCatalog) Lookup(name string) (*tools.Definition, bool) {
	definition, ok := c.definitions[name]
	return definition, ok
}

func (c *StepCatalog) CheckArguments(name string, args json.RawMessage) error {
	definition, ok := c.definitions[name]
	if !ok {
		return toolbroker.Errorf(toolbroker.ResultPolicyDenied, "unknown tool %q version %q", name, tools.Version)
	}
	if _, err := definition.Validate(args); err != nil {
		return toolbroker.Errorf(toolbroker.ResultInvalidArgument, "%v", err)
	}
	return nil
}
