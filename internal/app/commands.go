package app

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"

	"github.com/spf13/cobra"

	appconfig "github.com/anggasct/aura/internal/app/config"
	legacyconfig "github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/modules/broadcast"
	"github.com/anggasct/aura/internal/modules/handoff"
	"github.com/anggasct/aura/internal/modules/knowledge"
	"github.com/anggasct/aura/internal/modules/mcp"
	"github.com/anggasct/aura/internal/modules/profile"
	"github.com/anggasct/aura/internal/modules/schedule"
)

const (
	mcpTrustDirName  = "mcp"
	mcpTrustFileName = "trust.json"
)

func mountCommands(root *cobra.Command, gf *globalFlags) {
	root.AddCommand(knowledge.Commands(knowledgeOpener(gf))...)
	root.AddCommand(profile.Commands(profileOpener(gf))...)
	root.AddCommand(handoff.Commands(handoffOpener(gf))...)
	root.AddCommand(schedule.Commands(scheduleOpener(gf))...)
	root.AddCommand(broadcast.Commands(broadcastOpener(gf))...)
	root.AddCommand(mcp.Commands(mcpOpener(gf))...)
}

func mcpTrustRegistryPath(cfg *appconfig.Config) (string, error) {
	dataRoot, err := resolveDataRoot(cfg)
	if err != nil {
		return "", err
	}
	return filepath.Join(dataRoot, mcpTrustDirName, mcpTrustFileName), nil
}

func configuredMCPServers(cfg *appconfig.Config) []legacyconfig.MCPServer {
	if cfg == nil || cfg.MCP == nil {
		return nil
	}
	return cfg.MCP.Servers
}

func mcpOpener(gf *globalFlags) mcp.Opener {
	return func(cmd *cobra.Command, fn func(ctx context.Context, servers []legacyconfig.MCPServer, registry *mcp.FileTrustRegistry) error) error {
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
