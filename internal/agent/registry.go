package agent

import (
	"cmp"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/tools"
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
	return BuildWithMCP(overrides, knownTools, nil, modelRoutes)
}

func BuildWithMCP(overrides []config.AgentDefinition, knownTools, mcpServers, modelRoutes []string) (*Registry, error) {
	registry := newRegistry()
	for index := range builtins {
		definition := builtins[index].clone()
		definition.MemoryScope = MemoryScopePersona
		definition.Name = cmp.Or(definition.Name, definition.ID)
		definition.Audience = config.AudienceOwner
		definition.GroupContext = config.GroupContextPerPrincipal
		registry.upsert(&definition)
	}
	seen := make(map[string]struct{}, len(overrides))
	for index := range overrides {
		definition, err := buildOverride(&overrides[index], index, knownTools, mcpServers, modelRoutes)
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

func buildOverride(override *config.AgentDefinition, index int, knownTools, mcpServers, modelRoutes []string) (*Definition, error) {
	field := fmt.Sprintf("agents.definitions[%d]", index)
	if strings.TrimSpace(override.ID) == "" {
		return nil, codedError(ErrorCodeDefinitionInvalid, field+".id must not be empty")
	}
	if !validID.MatchString(override.ID) {
		return nil, codedError(ErrorCodeDefinitionInvalid, field+".id must be lowercase letters, digits, '-' or '_', start with a letter, and be at most 64 characters")
	}
	definition := Definition{
		ID:              override.ID,
		Description:     override.Description,
		ModelRoute:      override.ModelRoute,
		MemoryScope:     cmp.Or(override.MemoryScope, MemoryScopePersona),
		Limits:          Limits{TurnTimeout: time.Duration(override.Limits.TurnTimeout)},
		Audience:        cmp.Or(override.Audience, config.AudienceOwner),
		Pairing:         override.Pairing,
		PrincipalTools:  slices.Clone(override.PrincipalTools),
		PublicTools:     slices.Clone(override.PublicTools),
		ApprovalTimeout: time.Duration(override.ApprovalTimeout),
		Messages:        maps.Clone(override.Messages),
		GroupContext:    cmp.Or(override.GroupContext, config.GroupContextPerPrincipal),
		EvalDir:         override.Evals.Dir,
		Workflows:       slices.Clone(override.Workflows),
		Plugins:         slices.Clone(override.Plugins),

		Knowledge:          slices.Clone(override.Knowledge),
		PrincipalKnowledge: slices.Clone(override.PrincipalKnowledge),
		PublicKnowledge:    slices.Clone(override.PublicKnowledge),
	}
	if behavior := override.Evals.Behavior; behavior != nil {
		definition.EvalBehavior = &EvalBehavior{
			Required:            behavior.Required,
			MinScore:            cmp.Or(behavior.MinScore, DefaultEvalMinScore),
			JudgeRoute:          cmp.Or(behavior.JudgeRoute, DefaultEvalJudgeRoute),
			Schedule:            behavior.Schedule,
			EnforceOnRegression: behavior.EnforceOnRegression,
		}
	}
	if definition.MemoryScope != MemoryScopePersona && definition.MemoryScope != MemoryScopeShared {
		return nil, codedError(ErrorCodeDefinitionInvalid, fmt.Sprintf("%s.memory_scope must be %q or %q", field, MemoryScopePersona, MemoryScopeShared))
	}
	builtin, isBuiltin := builtinByID(override.ID)
	if isBuiltin {
		definition.Name = builtin.Name
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
	definition.Name = cmp.Or(override.Name, definition.Name, definition.ID)

	if !isBuiltin && strings.TrimSpace(definition.Instructions) == "" {
		return nil, codedError(ErrorCodeDefinitionInvalid, field+".instructions must not be empty")
	}
	if !isBuiltin && definition.Description == "" {
		return nil, codedError(ErrorCodeDefinitionInvalid, field+".description must not be empty")
	}
	for _, tool := range definition.Tools {
		if tools.IsMCPEntry(tool) {
			if !namesConfiguredMCPServer(tool, mcpServers) {
				return nil, codedError(ErrorCodePersonaInvalid, field+".tools: "+fmt.Sprintf("%q", tool)+" does not name a configured mcp server")
			}
			continue
		}
		if !slices.Contains(knownTools, tool) {
			return nil, codedError(ErrorCodeUnknownTool, field+".tools: unknown tool "+fmt.Sprintf("%q", tool))
		}
	}
	if err := validateToolCeilings(&definition, field, knownTools, mcpServers); err != nil {
		return nil, err
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
	if definition.EvalBehavior != nil && !slices.Contains(modelRoutes, definition.EvalBehavior.JudgeRoute) {
		return nil, codedError(ErrorCodeUnknownModelRoute, field+".evals.behavior.judge_route: unknown model route "+fmt.Sprintf("%q", definition.EvalBehavior.JudgeRoute))
	}
	if definition.Limits.TurnTimeout < 0 {
		return nil, codedError(ErrorCodeDefinitionInvalid, field+".limits.turn_timeout must not be negative")
	}
	return &definition, nil
}

func validateToolCeilings(definition *Definition, field string, knownTools, mcpServers []string) error {
	available := definition.Tools
	if len(available) == 0 {
		available = knownTools
	} else if len(definition.Knowledge) > 0 && !slices.Contains(available, tools.KBSearchToolName) {
		available = append(slices.Clone(available), tools.KBSearchToolName)
	}
	for _, tool := range definition.PrincipalTools {
		if tools.IsPluginEntry(tool) {
			if !namesListedPlugin(tool, definition.Plugins) {
				return codedError(ErrorCodePersonaInvalid, fmt.Sprintf("%s.principal_tools: %q does not name a plugin the persona lists", field, tool))
			}
			continue
		}
		if !listedInTools(tool, available, mcpServers) {
			return codedError(ErrorCodePersonaInvalid, fmt.Sprintf("%s.principal_tools: %q is not one of the persona's tools", field, tool))
		}
	}
	for _, tool := range definition.PublicTools {
		if tools.IsPluginEntry(tool) {
			return codedError(ErrorCodePersonaInvalid, fmt.Sprintf("%s.public_tools: %q is a plugin tool and plugin tools are never available to public principals", field, tool))
		}
		if tools.IsMCPEntry(tool) {
			return codedError(ErrorCodePersonaInvalid, fmt.Sprintf("%s.public_tools: %q is an mcp tool and mcp tools are never available to public principals", field, tool))
		}
		if !slices.Contains(available, tool) {
			return codedError(ErrorCodePersonaInvalid, fmt.Sprintf("%s.public_tools: %q is not one of the persona's tools", field, tool))
		}
		if tools.EffectTier(tool) {
			return codedError(ErrorCodePersonaInvalid, fmt.Sprintf("%s.public_tools: %q is an effect-tier tool", field, tool))
		}
	}
	return nil
}

func (d *Definition) AllowsPrincipalTool(toolName, server string) bool {
	return tools.ListsTool(d.PrincipalTools, toolName, server)
}

func listedInTools(entry string, available, servers []string) bool {
	if slices.Contains(available, entry) {
		return true
	}
	if !tools.IsMCPEntry(entry) {
		return false
	}
	for _, server := range servers {
		serverEntry := tools.MCPServerEntry(server)
		if strings.HasPrefix(entry, serverEntry+"_") && len(entry) > len(serverEntry)+1 && slices.Contains(available, serverEntry) {
			return true
		}
	}
	return false
}

func namesConfiguredMCPServer(entry string, servers []string) bool {
	for _, server := range servers {
		serverEntry := tools.MCPServerEntry(server)
		if entry == serverEntry || (strings.HasPrefix(entry, serverEntry+"_") && len(entry) > len(serverEntry)+1) {
			return true
		}
	}
	return false
}

func namesListedPlugin(entry string, plugins []string) bool {
	for _, plugin := range plugins {
		if entry == tools.PluginEntry(plugin) || tools.NamesPluginTool(entry, plugin) {
			return true
		}
	}
	return false
}
