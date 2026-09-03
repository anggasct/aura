package agent

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/anggasct/aura/internal/config"
)

// Registry holds the ordered definition set: builtins in declaration order,
// with config overrides replacing their builtin slot and config-only
// definitions appended in file order.
type Registry struct {
	definitions []Definition
	byID        map[string]int
}

func newRegistry() *Registry {
	return &Registry{byID: map[string]int{}}
}

func (r *Registry) upsert(definition *Definition) {
	if index, ok := r.byID[definition.key()]; ok {
		r.definitions[index] = *definition
		return
	}
	r.byID[definition.key()] = len(r.definitions)
	r.definitions = append(r.definitions, *definition)
}

// Lookup returns the definition registered under id.
func (r *Registry) Lookup(id string) (Definition, bool) {
	index, ok := r.byID[id]
	if !ok {
		return Definition{}, false
	}
	return r.definitions[index].clone(), true
}

// Definitions returns the registered definitions in stable order.
func (r *Registry) Definitions() []Definition {
	result := make([]Definition, 0, len(r.definitions))
	for _, definition := range r.definitions {
		result = append(result, definition.clone())
	}
	return result
}

// Resolve selects the definition for a unit of work. An explicit preference
// must match exactly. Otherwise the eligible set is every definition whose
// capability set covers required; the most-specific match wins — fewest
// declared capabilities beyond the required set — with ties broken by stable
// registry order. No eligible definition fails closed before any work starts.
func (r *Registry) Resolve(required []string, preferID *string) (Definition, error) {
	if preferID != nil {
		definition, ok := r.Lookup(*preferID)
		if !ok {
			return Definition{}, codedError(ErrorCodeNotFound, fmt.Sprintf("agent %q is not registered", *preferID))
		}
		return definition, nil
	}
	best := -1
	bestUnused := 0
	for index := range r.definitions {
		definition := &r.definitions[index]
		if !definition.supports(required) {
			continue
		}
		unused := definition.unusedCapabilityCount(required)
		if best < 0 || unused < bestUnused {
			best, bestUnused = index, unused
		}
	}
	if best < 0 {
		return Definition{}, codedError(ErrorCodeResolutionFailed, fmt.Sprintf("no agent covers required capabilities [%s]", strings.Join(required, ", ")))
	}
	return r.definitions[best].clone(), nil
}

// Build validates the compiled-in definitions against the configured
// tool registry and model roles, applies config overrides, and returns the
// startup registry. Any invalid definition aborts startup with a
// field-level error naming the offending entry.
func Build(overrides []config.AgentDefinition, knownTools, modelRoutes []string) (*Registry, error) {
	registry := newRegistry()
	for index := range builtins {
		definition := builtins[index].clone()
		registry.upsert(&definition)
	}
	for index := range overrides {
		definition, err := buildOverride(&overrides[index], index, knownTools, modelRoutes)
		if err != nil {
			return nil, err
		}
		registry.upsert(definition)
	}
	return registry, nil
}

func buildOverride(override *config.AgentDefinition, index int, knownTools, modelRoutes []string) (*Definition, error) {
	field := fmt.Sprintf("agents.definitions[%d]", index)
	if strings.TrimSpace(override.ID) == "" {
		return nil, codedError(ErrorCodeDefinitionInvalid, field+".id must not be empty")
	}
	definition := Definition{
		ID:          override.ID,
		Description: override.Description,
		ModelRoute:  override.ModelRoute,
		Limits:      Limits{TurnTimeout: time.Duration(override.Limits.TurnTimeout)},
	}
	builtin, isBuiltin := builtinByID(override.ID)
	if isBuiltin {
		definition.Tools = append([]string(nil), builtin.Tools...)
		definition.Instructions = builtin.Instructions
	} else {
		definition.Instructions = override.Instructions
	}
	if strings.TrimSpace(override.Instructions) != "" {
		definition.Instructions = override.Instructions
	}
	if len(override.Tools) > 0 {
		definition.Tools = append([]string(nil), override.Tools...)
	}
	definition.Capabilities = append([]string(nil), override.Capabilities...)

	if !isBuiltin && strings.TrimSpace(definition.Instructions) == "" {
		return nil, codedError(ErrorCodeDefinitionInvalid, field+".instructions must not be empty")
	}
	if !isBuiltin && definition.Description == "" {
		return nil, codedError(ErrorCodeDefinitionInvalid, field+".description must not be empty")
	}
	for _, tool := range definition.Tools {
		if !slices.Contains(knownTools, tool) {
			return nil, codedError(ErrorCodeUnknownTool, field+".tools: unknown tool "+fmt.Sprintf("%q", tool))
		}
	}
	for _, capability := range definition.Capabilities {
		if !knownCapability(capability) {
			return nil, codedError(ErrorCodeUnknownCapability, field+".capabilities: unknown capability "+fmt.Sprintf("%q", capability))
		}
	}
	if definition.ModelRoute != "" && !slices.Contains(modelRoutes, definition.ModelRoute) {
		return nil, codedError(ErrorCodeUnknownModelRoute, field+".model_route: unknown model route "+fmt.Sprintf("%q", definition.ModelRoute))
	}
	if definition.Limits.TurnTimeout < 0 {
		return nil, codedError(ErrorCodeDefinitionInvalid, field+".limits.turn_timeout must not be negative")
	}
	return &definition, nil
}
