package config

import (
	legacyconfig "github.com/anggasct/aura/internal/config"
)

type Config struct {
	Version      int                                `json:"version"`
	Runtime      legacyconfig.Runtime               `json:"runtime"`
	Capabilities legacyconfig.Capabilities          `json:"capabilities"`
	Tools        *legacyconfig.Tools                `json:"tools,omitempty"`
	Agents       *legacyconfig.Agents               `json:"agents,omitempty"`
	Workflows    *legacyconfig.Workflows            `json:"workflows,omitempty"`
	MCP          *legacyconfig.MCP                  `json:"mcp,omitempty"`
	Plugins      *legacyconfig.Plugins              `json:"plugins,omitempty"`
	Server       legacyconfig.Server                `json:"server"`
	Logging      legacyconfig.Logging               `json:"logging"`
	Models       legacyconfig.Models                `json:"models"`
	ModelRoutes  map[string]legacyconfig.ModelRoute `json:"model_routes,omitempty"`
	Storage      legacyconfig.Storage               `json:"storage"`
	Telemetry    legacyconfig.Telemetry             `json:"telemetry"`
	Usage        legacyconfig.Usage                 `json:"usage"`
	Health       legacyconfig.Health                `json:"health"`
	Terminal     legacyconfig.Terminal              `json:"terminal"`
	Webhook      legacyconfig.Webhook               `json:"webhook"`
	Broadcast    legacyconfig.Broadcast             `json:"broadcast"`
	Scheduler    legacyconfig.Scheduler             `json:"scheduler"`
	Memory       legacyconfig.Memory                `json:"memory"`
	Skills       *legacyconfig.Skills               `json:"skills,omitempty"`
	Context      *legacyconfig.Context              `json:"context,omitempty"`
	Profile      *legacyconfig.Profile              `json:"profile,omitempty"`
	Owner        *legacyconfig.Owner                `json:"owner,omitempty"`
	Children     *legacyconfig.Children             `json:"children,omitempty"`
	Access       *legacyconfig.Access               `json:"access,omitempty"`
	Sync         *legacyconfig.Sync                 `json:"sync,omitempty"`
	Vision       *legacyconfig.Vision               `json:"vision,omitempty"`
	Knowledge    *legacyconfig.Knowledge            `json:"knowledge,omitempty"`
	Interpreter  legacyconfig.Interpreter           `json:"interpreter"`
	Channels     legacyconfig.Channels              `json:"channels"`
	Bindings     []legacyconfig.Binding             `json:"bindings,omitempty"`
}

func Default() Config {
	return Config{
		Version:      1,
		Runtime:      legacyconfig.DefaultRuntime(),
		Capabilities: legacyconfig.DefaultCapabilities(),
		Agents:       legacyconfig.DefaultAgents(),
		Workflows:    legacyconfig.DefaultWorkflows(),
		Tools:        legacyconfig.DefaultTools(),
		Server:       legacyconfig.DefaultServer(),
		Logging:      legacyconfig.DefaultLogging(),
		Models:       legacyconfig.DefaultModels(),
		Storage:      legacyconfig.DefaultStorage(),
		Telemetry:    legacyconfig.DefaultTelemetry(),
		Usage:        legacyconfig.DefaultUsage(),
		Health:       legacyconfig.DefaultHealth(),
		Terminal:     legacyconfig.DefaultTerminal(),
		Webhook:      legacyconfig.DefaultWebhook(),
		Channels:     legacyconfig.DefaultChannels(),
		Broadcast:    legacyconfig.DefaultBroadcast(),
		Scheduler:    legacyconfig.DefaultScheduler(),
		Memory:       legacyconfig.DefaultMemory(),
		Interpreter:  legacyconfig.DefaultInterpreter(),
		Skills:       legacyconfig.DefaultSkills(),
		Sync:         legacyconfig.DefaultSync(),
		Vision:       legacyconfig.DefaultVision(),
		Context:      legacyconfig.DefaultContext(),
		Profile:      legacyconfig.DefaultProfile(),
		Children:     legacyconfig.DefaultChildren(),
		Access:       legacyconfig.DefaultAccess(),
	}
}
