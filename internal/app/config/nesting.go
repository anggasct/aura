package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/anggasct/aura/internal/platform/configkit"
	"github.com/anggasct/aura/internal/platform/storage"
)

func ContainsPath(outer, inner string) bool {
	relative, err := filepath.Rel(resolvedPath(outer), resolvedPath(inner))
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func resolvedPath(path string) string {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return filepath.Clean(path)
	}
	existing, rest := absolute, ""
	for {
		resolved, err := filepath.EvalSymlinks(existing)
		if err == nil {
			return filepath.Join(resolved, rest)
		}
		parent := filepath.Dir(existing)
		if parent == existing || !os.IsNotExist(err) {
			return absolute
		}
		rest = filepath.Join(filepath.Base(existing), rest)
		existing = parent
	}
}

func validateWorkspaceNesting(cfg *Config) error {
	if cfg == nil || cfg.Tools == nil || cfg.Tools.Workspace == "" {
		return nil
	}
	dataRoot, err := storage.ResolveDataRoot(&cfg.Storage)
	if err != nil {
		return nil
	}
	workspace, data := resolvedPath(cfg.Tools.Workspace), resolvedPath(dataRoot)
	if !ContainsPath(workspace, data) && !ContainsPath(data, workspace) {
		return nil
	}
	return configkit.Invalid("tools.workspace", fmt.Sprintf(
		"the workspace %s and the data directory %s are the same directory or one holds the other; set tools.workspace to a directory outside the data directory, or set storage.path to one outside the workspace",
		workspace, data))
}
