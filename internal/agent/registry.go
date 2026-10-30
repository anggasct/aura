package agent

import (
	"cmp"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/anggasct/aura/internal/config"
)

var validID = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)

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

func (r *Registry) Lookup(id string) (Definition, bool) {
	index, ok := r.byID[id]
	if !ok {
		return Definition{}, false
	}
	return r.definitions[index].clone(), true
}

func (r *Registry) RecallScope(id string) ([]string, bool) {
	index, ok := r.byID[id]
	if !ok {
		return nil, false
	}
	if r.definitions[index].MemoryScope != MemoryScopeShared {
		return []string{id}, true
	}
	readable := make([]string, 0, len(r.definitions))
	for i := range r.definitions {
		if r.definitions[i].MemoryScope == MemoryScopeShared {
			readable = append(readable, r.definitions[i].ID)
		}
	}
	return readable, true
}

func (r *Registry) Definitions() []Definition {
	result := make([]Definition, 0, len(r.definitions))
	for i := range r.definitions {
		result = append(result, r.definitions[i].clone())
	}
	return result
}

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

func Build(overrides []config.AgentDefinition, knownTools, modelRoutes []string) (*Registry, error) {
	registry := newRegistry()
	for index := range builtins {
		definition := builtins[index].clone()
		definition.MemoryScope = MemoryScopePersona
		registry.upsert(&definition)
	}
	seen := make(map[string]struct{}, len(overrides))
	for index := range overrides {
		definition, err := buildOverride(&overrides[index], index, knownTools, modelRoutes)
		if err != nil {
			return nil, err
		}
		if _, duplicate := seen[definition.ID]; duplicate {
			return nil, codedError(ErrorCodeDuplicateID, fmt.Sprintf("agents.definitions[%d].id: duplicate agent id %q", index, definition.ID))
		}
		seen[definition.ID] = struct{}{}
		registry.upsert(definition)
	}
	return registry, nil
}

func buildOverride(override *config.AgentDefinition, index int, knownTools, modelRoutes []string) (*Definition, error) {
	field := fmt.Sprintf("agents.definitions[%d]", index)
	if strings.TrimSpace(override.ID) == "" {
		return nil, codedError(ErrorCodeDefinitionInvalid, field+".id must not be empty")
	}
	if !validID.MatchString(override.ID) {
		return nil, codedError(ErrorCodeDefinitionInvalid, field+".id must be lowercase letters, digits, '-' or '_', start with a letter, and be at most 64 characters")
	}
	definition := Definition{
		ID:          override.ID,
		Description: override.Description,
		ModelRoute:  override.ModelRoute,
		MemoryScope: cmp.Or(override.MemoryScope, MemoryScopePersona),
		Limits:      Limits{TurnTimeout: time.Duration(override.Limits.TurnTimeout)},
	}
	if definition.MemoryScope != MemoryScopePersona && definition.MemoryScope != MemoryScopeShared {
		return nil, codedError(ErrorCodeDefinitionInvalid, fmt.Sprintf("%s.memory_scope must be %q or %q", field, MemoryScopePersona, MemoryScopeShared))
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
	if override.Soul != "" {
		definition.Instructions = override.Soul
		definition.SoulFile = override.SoulFile
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
	seenCapabilities := make([]string, 0, len(definition.Capabilities))
	for _, capability := range definition.Capabilities {
		if !knownCapability(capability) {
			return nil, codedError(ErrorCodeUnknownCapability, field+".capabilities: unknown capability "+fmt.Sprintf("%q", capability))
		}
		if slices.Contains(seenCapabilities, capability) {
			return nil, codedError(ErrorCodeDefinitionInvalid, field+".capabilities: duplicate capability "+fmt.Sprintf("%q", capability))
		}
		seenCapabilities = append(seenCapabilities, capability)
	}
	if definition.ModelRoute != "" && !slices.Contains(modelRoutes, definition.ModelRoute) {
		return nil, codedError(ErrorCodeUnknownModelRoute, field+".model_route: unknown model route "+fmt.Sprintf("%q", definition.ModelRoute))
	}
	if definition.Limits.TurnTimeout < 0 {
		return nil, codedError(ErrorCodeDefinitionInvalid, field+".limits.turn_timeout must not be negative")
	}
	return &definition, nil
}
