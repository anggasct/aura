package tools

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

const (
	MCPToolPrefix       = "mcp_"
	OperationToolPrefix = "aura_"

	OperationSearchToolName = "aura_operation_search"
)

func IsOperationEntry(name string) bool {
	return strings.HasPrefix(name, OperationToolPrefix)
}

func IsMCPEntry(name string) bool {
	return strings.HasPrefix(name, MCPToolPrefix)
}

func MCPServerEntry(server string) string {
	return MCPToolPrefix + server
}

type PluginRef struct {
	ID          string
	Description string
}

const (
	PluginToolPrefix     = "plugin_"
	MaxPluginToolWireLen = 64
)

func IsPluginEntry(name string) bool {
	return strings.HasPrefix(name, PluginToolPrefix)
}

func PluginEntry(plugin string) string {
	return PluginToolPrefix + plugin
}

func PluginToolID(plugin, tool string) string {
	return plugin + "." + tool
}

func PluginToolWireName(plugin, tool string) string {
	return PluginToolPrefix + plugin + "_" + tool
}

func NamesPluginTool(entry, plugin string) bool {
	prefix := PluginEntry(plugin) + "_"
	return strings.HasPrefix(entry, prefix) && len(entry) > len(prefix)
}

func ListsPluginTool(entries []string, name, plugin string) bool {
	if slices.Contains(entries, name) {
		return true
	}
	return plugin != "" && slices.Contains(entries, PluginEntry(plugin))
}

func ListsTool(entries []string, name, server string) bool {
	if slices.Contains(entries, name) {
		return true
	}
	return server != "" && slices.Contains(entries, MCPServerEntry(server))
}

func ListedInCeiling(ceiling []string, name string, definition *Definition) bool {
	if definition.Plugin != nil {
		return ListsPluginTool(ceiling, name, definition.Plugin.ID)
	}
	return ListsTool(ceiling, name, definition.Server)
}

const (
	Version          = "v1"
	KBSearchToolName = "kb_search"
	HandoffToolName  = "handoff_to_owner"
)

type Definition struct {
	Name                 string
	Version              string
	Channel              string
	Server               string
	Plugin               *PluginRef
	Schema               json.RawMessage
	RequiredCapabilities []string
	RequiresApproval     bool
	Effectful            bool
	OptIn                bool
	OwnerOptIn           bool
	ExternalSink         bool
	NoStandingGrant      bool
	Orchestration        bool
	RepliesToRequester   bool
	ExactDisplay         bool
	Risk                 Risk
	Operation            string
	FreshApproval        bool
	OwnerNotice          bool
	UntrustedOutput      bool
	Description          string
	Display              []string
	Validator            func(raw json.RawMessage) (json.RawMessage, error)
}

func (d *Definition) IsExternalSink() bool {
	return d.ExternalSink || d.Server != "" || d.Plugin != nil
}

func (d *Definition) RunsAlone() bool {
	return d.Effectful || d.Orchestration
}

func (d *Definition) Key() string {
	return d.Name + "@" + d.Version
}

func (d *Definition) Validate(raw json.RawMessage) (json.RawMessage, error) {
	validated, err := d.validate(raw)
	if err != nil {
		return nil, err
	}
	if err := rejectAmbiguousKeys(raw); err != nil {
		return nil, err
	}
	return validated, nil
}

func (d *Definition) validate(raw json.RawMessage) (json.RawMessage, error) {
	if d.Validator == nil {
		return nil, fmt.Errorf("tool %q has no validator", d.Name)
	}
	return d.Validator(raw)
}
