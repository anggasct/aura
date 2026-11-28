package app

import (
	"fmt"
	"os"

	appconfig "github.com/anggasct/aura/internal/app/config"
	"github.com/anggasct/aura/internal/modules/mcp"
	"github.com/anggasct/aura/internal/platform/configkit"
	"github.com/anggasct/aura/internal/platform/storage"
)

func checkMCPWorkingDir(cfg *appconfig.Config, plugins []mcp.PluginServer) error {
	if !runsStdioServer(cfg, plugins) {
		return nil
	}
	workingDir, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("resolve the working directory: %w", err)
	}
	dataRoot, err := storage.ResolveDataRoot(&cfg.Storage)
	if err != nil {
		return err
	}
	if !appconfig.ContainsPath(workingDir, dataRoot) {
		return nil
	}
	return configkit.Invalid("mcp", fmt.Sprintf("a stdio server gets the working directory %s read-write, which holds the data directory %s; start the daemon from another directory", workingDir, dataRoot))
}

func runsStdioServer(cfg *appconfig.Config, plugins []mcp.PluginServer) bool {
	for _, server := range configuredMCPServers(cfg) {
		if server.Transport == mcp.TransportStdio {
			return true
		}
	}
	for i := range plugins {
		if plugins[i].Server.Transport == mcp.TransportStdio {
			return true
		}
	}
	return false
}
