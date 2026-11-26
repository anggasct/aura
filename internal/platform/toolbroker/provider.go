package toolbroker

import (
	"slices"

	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/tools"
)

type ToolRegistration struct {
	Definition tools.Definition
	Adapter    Adapter
	Rule       approval.Rule
}

type ToolProvider interface {
	Tools() []ToolRegistration
}

func (b *Broker) RegisterProvider(provider ToolProvider) error {
	if provider == nil {
		return errorf(ResultInvalidArgument, "tool provider must not be nil")
	}
	registrations := provider.Tools()
	if len(registrations) == 0 {
		return nil
	}

	seenKeys := make(map[string]struct{}, len(registrations))
	seenNames := make(map[string]struct{}, len(registrations))
	defs := make([]tools.Definition, len(registrations))
	rules := make([]approval.Rule, len(registrations))
	for i := range registrations {
		reg := &registrations[i]
		if reg.Adapter == nil {
			return errorf(ResultInvalidArgument, "adapter must not be nil")
		}
		if reg.Definition.Name == "" || reg.Definition.Version == "" {
			return errorf(ResultInvalidArgument, "tool definition requires name and version")
		}
		if reg.Rule.ToolName != reg.Definition.Name || reg.Rule.ToolVersion != reg.Definition.Version {
			return errorf(ResultInvalidArgument, "rule tool %q version %q does not match definition %q version %q", reg.Rule.ToolName, reg.Rule.ToolVersion, reg.Definition.Name, reg.Definition.Version)
		}
		key := reg.Definition.Key()
		if _, dup := seenKeys[key]; dup {
			return errorf(ResultInvalidArgument, "duplicate tool definition %q", key)
		}
		seenKeys[key] = struct{}{}
		if _, dup := seenNames[reg.Rule.ToolName]; dup {
			return errorf(ResultInvalidArgument, "duplicate tool rule %q", reg.Rule.ToolName)
		}
		seenNames[reg.Rule.ToolName] = struct{}{}

		defCopy := reg.Definition
		defCopy.Schema = slices.Clone(reg.Definition.Schema)
		defCopy.RequiredCapabilities = slices.Clone(reg.Definition.RequiredCapabilities)
		defs[i] = defCopy
		rules[i] = reg.Rule
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	if err := b.engine.RegisterRules(rules); err != nil {
		return err
	}
	for i := range defs {
		defCopy := defs[i]
		key := defCopy.Key()
		b.definitions[key] = &defCopy
		b.adapters[key] = registrations[i].Adapter
	}
	return nil
}
