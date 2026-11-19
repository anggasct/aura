package config

import (
	legacyconfig "github.com/anggasct/aura/internal/config"
	auraagent "github.com/anggasct/aura/internal/platform/agent"
	"github.com/anggasct/aura/internal/platform/capability"
	"github.com/anggasct/aura/internal/platform/health"
	"github.com/anggasct/aura/internal/platform/logging"
	"github.com/anggasct/aura/internal/platform/model"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/server"
	"github.com/anggasct/aura/internal/platform/telemetry"
	"github.com/anggasct/aura/internal/platform/usage"
	"github.com/anggasct/aura/internal/platform/vision"
)

type Config struct {
	Version      int                         `json:"version"`
	Runtime      legacyconfig.Runtime        `json:"runtime"`
	Capabilities capability.Config           `json:"capabilities"`
	Tools        *legacyconfig.Tools         `json:"tools,omitempty"`
	Agents       *auraagent.Config           `json:"agents,omitempty"`
	Workflows    *legacyconfig.Workflows     `json:"workflows,omitempty"`
	MCP          *legacyconfig.MCP           `json:"mcp,omitempty"`
	Plugins      *legacyconfig.Plugins       `json:"plugins,omitempty"`
	Server       server.Config               `json:"server"`
	Logging      logging.Config              `json:"logging"`
	Models       model.Config                `json:"models"`
	ModelRoutes  map[string]model.ModelRoute `json:"model_routes,omitempty"`
	Storage      legacyconfig.Storage        `json:"storage"`
	Telemetry    telemetry.Config            `json:"telemetry"`
	Usage        usage.Config                `json:"usage"`
	Health       health.Config               `json:"health"`
	Terminal     legacyconfig.Terminal       `json:"terminal"`
	Webhook      legacyconfig.Webhook        `json:"webhook"`
	Broadcast    legacyconfig.Broadcast      `json:"broadcast"`
	Scheduler    legacyconfig.Scheduler      `json:"scheduler"`
	Memory       legacyconfig.Memory         `json:"memory"`
	Skills       *legacyconfig.Skills        `json:"skills,omitempty"`
	Context      *legacyconfig.Context       `json:"context,omitempty"`
	Profile      *legacyconfig.Profile       `json:"profile,omitempty"`
	Owner        *principal.Owner            `json:"owner,omitempty"`
	Children     *legacyconfig.Children      `json:"children,omitempty"`
	Access       *principal.Config           `json:"access,omitempty"`
	Sync         *legacyconfig.Sync          `json:"sync,omitempty"`
	Vision       *vision.Config              `json:"vision,omitempty"`
	Knowledge    *legacyconfig.Knowledge     `json:"knowledge,omitempty"`
	Interpreter  legacyconfig.Interpreter    `json:"interpreter"`
	Channels     legacyconfig.Channels       `json:"channels"`
	Bindings     auraagent.Bindings          `json:"bindings,omitempty"`
}

func Default() Config {
	defaultAgents := auraagent.Default()
	defaultAccess := principal.Default()
	defaultVision := vision.Default()
	return Config{
		Version:      1,
		Runtime:      legacyconfig.DefaultRuntime(),
		Capabilities: capability.Default(),
		Agents:       &defaultAgents,
		Workflows:    legacyconfig.DefaultWorkflows(),
		Tools:        legacyconfig.DefaultTools(),
		Server:       server.Default(),
		Logging:      logging.Default(),
		Models:       model.Default(),
		Storage:      legacyconfig.DefaultStorage(),
		Telemetry:    telemetry.Default(),
		Usage:        usage.Default(),
		Health:       health.Default(),
		Terminal:     legacyconfig.DefaultTerminal(),
		Webhook:      legacyconfig.DefaultWebhook(),
		Channels:     legacyconfig.DefaultChannels(),
		Broadcast:    legacyconfig.DefaultBroadcast(),
		Scheduler:    legacyconfig.DefaultScheduler(),
		Memory:       legacyconfig.DefaultMemory(),
		Interpreter:  legacyconfig.DefaultInterpreter(),
		Skills:       legacyconfig.DefaultSkills(),
		Sync:         legacyconfig.DefaultSync(),
		Vision:       &defaultVision,
		Context:      legacyconfig.DefaultContext(),
		Profile:      legacyconfig.DefaultProfile(),
		Children:     legacyconfig.DefaultChildren(),
		Access:       &defaultAccess,
	}
}
