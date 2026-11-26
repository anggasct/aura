package app

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"

	"github.com/spf13/cobra"

	appconfig "github.com/anggasct/aura/internal/app/config"
	"github.com/anggasct/aura/internal/modules/broadcast"
	"github.com/anggasct/aura/internal/modules/codeexec"
	"github.com/anggasct/aura/internal/modules/handoff"
	"github.com/anggasct/aura/internal/modules/knowledge"
	"github.com/anggasct/aura/internal/modules/mcp"
	"github.com/anggasct/aura/internal/modules/memory"
	"github.com/anggasct/aura/internal/modules/plugin"
	"github.com/anggasct/aura/internal/modules/profile"
	"github.com/anggasct/aura/internal/modules/schedule"
	"github.com/anggasct/aura/internal/modules/skills"
	"github.com/anggasct/aura/internal/modules/workflow"
)

const (
	mcpTrustDirName  = "mcp"
	mcpTrustFileName = "trust.json"
)

func mountCommands(root *cobra.Command, gf *globalFlags) {
	root.AddCommand(knowledge.Commands(knowledgeOpener(gf))...)
	root.AddCommand(memory.Commands(memoryOpener(gf))...)
	root.AddCommand(profile.Commands(profileOpener(gf))...)
	root.AddCommand(handoff.Commands(handoffOpener(gf))...)
	root.AddCommand(schedule.Commands(scheduleOpener(gf))...)
	root.AddCommand(broadcast.Commands(broadcastOpener(gf))...)
	root.AddCommand(plugin.Commands(pluginOpener(gf))...)
	root.AddCommand(mcp.Commands(mcpOpener(gf))...)
	root.AddCommand(skills.Commands(skillsOpener(gf))...)
	root.AddCommand(workflow.Commands(workflowOpener(gf))...)
	root.AddCommand(codeexec.Commands(interpreterOpener(gf))...)
}

func mcpTrustRegistryPath(cfg *appconfig.Config) (string, error) {
	dataRoot, err := resolveDataRoot(cfg)
	if err != nil {
		return "", err
	}
	return filepath.Join(dataRoot, mcpTrustDirName, mcpTrustFileName), nil
}

func configuredMCPServers(cfg *appconfig.Config) []mcp.ServerConfig {
	if cfg == nil || cfg.MCP == nil {
		return nil
	}
	return cfg.MCP.Servers
}

func mcpOpener(gf *globalFlags) mcp.Opener {
	return func(cmd *cobra.Command, fn func(ctx context.Context, servers []mcp.ServerConfig, registry *mcp.FileTrustRegistry) error) error {
		result, err := appconfig.Load(gf.configPath)
		if err != nil {
			return err
		}
		trustPath, err := mcpTrustRegistryPath(result.Config)
		if err != nil {
			return err
		}
		registry, err := mcp.NewFileTrustRegistry(trustPath)
		if err != nil {
			return err
		}
		return fn(cmd.Context(), configuredMCPServers(result.Config), registry)
	}
}

func skillsOpener(gf *globalFlags) skills.Opener {
	return func(cmd *cobra.Command, fn func(ctx context.Context, engine *skills.Engine, defaultRoot string) error) error {
		result, err := appconfig.Load(gf.configPath)
		if err != nil {
			return err
		}
		return withApp(cmd, &commandInput{result: &result, parts: []Part{PartSkills}}, func(ctx context.Context, app *App) error {
			return fn(ctx, app.skillsEngine, app.skillsRoot)
		})
	}
}

func knowledgeOpener(gf *globalFlags) knowledge.Opener {
	return func(cmd *cobra.Command, fn func(ctx context.Context, logger *slog.Logger, service *knowledge.Service) error) error {
		result, err := appconfig.Load(gf.configPath)
		if err != nil {
			return err
		}
		return withApp(cmd, &commandInput{result: &result, parts: []Part{PartKnowledge}}, func(ctx context.Context, app *App) error {
			return fn(ctx, app.logger, app.knowledge)
		})
	}
}

func memoryOpener(gf *globalFlags) memory.Opener {
	return func(cmd *cobra.Command, fn func(ctx context.Context, logger *slog.Logger, service *memory.Service) error) error {
		result, err := appconfig.Load(gf.configPath)
		if err != nil {
			return err
		}
		return withApp(cmd, &commandInput{result: &result, parts: []Part{PartMemory}}, func(ctx context.Context, app *App) error {
			return fn(ctx, app.logger, app.memory)
		})
	}
}

func profileOpener(gf *globalFlags) profile.Opener {
	return func(cmd *cobra.Command, fn func(ctx context.Context, service *profile.Service) error) error {
		result, err := appconfig.Load(gf.configPath)
		if err != nil {
			return err
		}
		if result.Config.Profile == nil || !result.Config.Profile.Enabled {
			return errors.New("user profiling is not enabled")
		}
		return withApp(cmd, &commandInput{result: &result, parts: []Part{PartProfile}}, func(ctx context.Context, app *App) error {
			return fn(ctx, app.profile)
		})
	}
}

func pluginOpener(gf *globalFlags) plugin.Opener {
	return func(cmd *cobra.Command, fn func(ctx context.Context, service *plugin.Service, audit plugin.AuditRecorder) error) error {
		result, err := appconfig.Load(gf.configPath)
		if err != nil {
			return err
		}
		return withApp(cmd, &commandInput{result: &result, parts: []Part{PartPlugins}}, func(ctx context.Context, app *App) error {
			return fn(ctx, app.plugins, &pluginAuditAdapter{db: app.db})
		})
	}
}
