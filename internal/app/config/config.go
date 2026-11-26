package config

import (
	legacyconfig "github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/modules/broadcast"
	"github.com/anggasct/aura/internal/modules/compaction"
	"github.com/anggasct/aura/internal/modules/knowledge"
	"github.com/anggasct/aura/internal/modules/mcp"
	"github.com/anggasct/aura/internal/modules/plugin"
	"github.com/anggasct/aura/internal/modules/profile"
	"github.com/anggasct/aura/internal/modules/schedule"
	"github.com/anggasct/aura/internal/modules/skills"
	auraagent "github.com/anggasct/aura/internal/platform/agent"
	"github.com/anggasct/aura/internal/platform/capability"
	"github.com/anggasct/aura/internal/platform/health"
	"github.com/anggasct/aura/internal/platform/logging"
	"github.com/anggasct/aura/internal/platform/model"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/server"
	"github.com/anggasct/aura/internal/platform/storage"
	"github.com/anggasct/aura/internal/platform/telemetry"
	runtimeengine "github.com/anggasct/aura/internal/platform/turn/engine"
	"github.com/anggasct/aura/internal/platform/usage"
	"github.com/anggasct/aura/internal/platform/vision"
	platformwebhook "github.com/anggasct/aura/internal/platform/webhook"
)

type Config struct {
	Version      int                         `json:"version"`
	Runtime      runtimeengine.Config        `json:"runtime"`
	Capabilities capability.Config           `json:"capabilities"`
	Tools        *legacyconfig.Tools         `json:"tools,omitempty"`
	Agents       *auraagent.Config           `json:"agents,omitempty"`
	Workflows    *legacyconfig.Workflows     `json:"workflows,omitempty"`
	MCP          *mcp.Config                 `json:"mcp,omitempty"`
	Plugins      *plugin.Config              `json:"plugins,omitempty"`
	Server       server.Config               `json:"server"`
	Logging      logging.Config              `json:"logging"`
	Models       model.Config                `json:"models"`
	ModelRoutes  map[string]model.ModelRoute `json:"model_routes,omitempty"`
	Storage      storage.Config              `json:"storage"`
	Telemetry    telemetry.Config            `json:"telemetry"`
	Usage        usage.Config                `json:"usage"`
	Health       health.Config               `json:"health"`
	Terminal     legacyconfig.Terminal       `json:"terminal"`
	Webhook      platformwebhook.Config      `json:"webhook"`
	Broadcast    broadcast.Config            `json:"broadcast"`
	Scheduler    schedule.Config             `json:"scheduler"`
	Memory       legacyconfig.Memory         `json:"memory"`
	Skills       *skills.Config              `json:"skills,omitempty"`
	Context      *compaction.Config          `json:"context,omitempty"`
	Profile      *profile.Config             `json:"profile,omitempty"`
	Owner        *principal.Owner            `json:"owner,omitempty"`
	Children     *legacyconfig.Children      `json:"children,omitempty"`
	Access       *principal.Config           `json:"access,omitempty"`
	Sync         *legacyconfig.Sync          `json:"sync,omitempty"`
	Vision       *vision.Config              `json:"vision,omitempty"`
	Knowledge    *knowledge.Config           `json:"knowledge,omitempty"`
	Interpreter  legacyconfig.Interpreter    `json:"interpreter"`
	Channels     legacyconfig.Channels       `json:"channels"`
	Bindings     auraagent.Bindings          `json:"bindings,omitempty"`
}

func Default() Config {
	defaultAgents := auraagent.Default()
	defaultAccess := principal.Default()
	defaultVision := vision.Default()
	defaultSkills := skills.Default()
	defaultContext := compaction.Default()
	defaultProfile := profile.Default()
	return Config{
		Version:      1,
		Runtime:      runtimeengine.Default(),
		Capabilities: capability.Default(),
		Agents:       &defaultAgents,
		Workflows:    legacyconfig.DefaultWorkflows(),
		Tools:        legacyconfig.DefaultTools(),
		Server:       server.Default(),
		Logging:      logging.Default(),
		Models:       model.Default(),
		Storage:      storage.Default(),
		Telemetry:    telemetry.Default(),
		Usage:        usage.Default(),
		Health:       health.Default(),
		Terminal:     legacyconfig.DefaultTerminal(),
		Webhook:      platformwebhook.Default(),
		Channels:     legacyconfig.DefaultChannels(),
		Broadcast:    broadcast.Default(),
		Scheduler:    schedule.Default(),
		Memory:       legacyconfig.DefaultMemory(),
		Interpreter:  legacyconfig.DefaultInterpreter(),
		Skills:       &defaultSkills,
		Sync:         legacyconfig.DefaultSync(),
		Vision:       &defaultVision,
		Context:      &defaultContext,
		Profile:      &defaultProfile,
		Children:     legacyconfig.DefaultChildren(),
		Access:       &defaultAccess,
	}
}
