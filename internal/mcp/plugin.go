package mcp

import (
	"slices"

	"github.com/anggasct/aura/internal/config"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	pluginKeyPrefix  = "plugin:"
	pluginNamePrefix = "plugin-"
)

type PluginTool struct {
	Name        string
	Description string
}

type PluginServer struct {
	ID     string
	Server config.MCPServer
	Tools  []PluginTool
}

type PluginStatus struct {
	ID        string
	Code      ErrorCode
	Connected bool
	Tools     []string
	Drift     []string
}

type startTarget struct {
	key    string
	server *config.MCPServer
	plugin *PluginServer
}

func pluginKey(id string) string {
	return pluginKeyPrefix + id
}

func pluginServerName(id string) string {
	return pluginNamePrefix + id
}

func (p *PluginServer) approvedDescription(tool string) string {
	for i := range p.Tools {
		if p.Tools[i].Name == tool {
			return p.Tools[i].Description
		}
	}
	return ""
}

func clonePluginServer(plugin *PluginServer) PluginServer {
	return PluginServer{ID: plugin.ID, Server: plugin.Server, Tools: slices.Clone(plugin.Tools)}
}

func clonePluginServers(plugins []PluginServer) []PluginServer {
	cloned := make([]PluginServer, 0, len(plugins))
	for i := range plugins {
		cloned = append(cloned, clonePluginServer(&plugins[i]))
	}
	return cloned
}

func validatePluginServers(plugins []PluginServer, cfg *config.MCP) error {
	seen := make(map[string]bool, len(plugins))
	for i := range plugins {
		plugin := &plugins[i]
		if !config.ValidPluginID(plugin.ID) || seen[plugin.ID] {
			return Errorf(ErrConfigInvalid, "plugin ids must be valid and unique")
		}
		seen[plugin.ID] = true
		for i := range cfg.Servers {
			if cfg.Servers[i].Name == pluginServerName(plugin.ID) {
				return Errorf(ErrConfigInvalid, "plugin %q collides with a configured server name", plugin.ID)
			}
		}
		server := plugin.Server
		if err := config.ValidateMCPServer(&server, "plugin backend"); err != nil {
			return Errorf(ErrConfigInvalid, "plugin %q has an invalid backend", plugin.ID)
		}
		if server.Transport == config.MCPTransportLegacySSE {
			return Errorf(ErrConfigInvalid, "plugin %q uses an unsupported transport", plugin.ID)
		}
		names := make(map[string]bool, len(plugin.Tools))
		for _, tool := range plugin.Tools {
			if tool.Name == "" || names[tool.Name] {
				return Errorf(ErrConfigInvalid, "plugin %q lists an invalid tool", plugin.ID)
			}
			names[tool.Name] = true
		}
	}
	return nil
}

func splitApprovedTools(discovered []DiscoveredTool, approved []PluginTool) (exposed []DiscoveredTool, drift []string) {
	for i := range discovered {
		if slices.ContainsFunc(approved, func(tool PluginTool) bool { return tool.Name == discovered[i].Name }) {
			exposed = append(exposed, discovered[i])
			continue
		}
		drift = append(drift, discovered[i].Name)
	}
	return exposed, drift
}

func (m *Manager) SetPluginTransport(id string, transport sdk.Transport) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.customTransports[pluginKey(id)] = transport
}

func (m *Manager) PluginStatuses() []PluginStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	statuses := make([]PluginStatus, 0, len(m.plugins))
	for i := range m.plugins {
		id := m.plugins[i].ID
		key := pluginKey(id)
		status := PluginStatus{ID: id, Code: ErrServerUnavailable}
		if client, running := m.clients[key]; running {
			status.Code = ""
			status.Connected = client.Connected()
			status.Tools = slices.Clone(m.registeredTools[key])
			status.Drift = slices.Clone(m.pluginDrift[id])
		} else if code, failed := m.failures[key]; failed {
			status.Code = code
		}
		statuses = append(statuses, status)
	}
	return statuses
}
