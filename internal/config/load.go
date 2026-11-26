package config

import (
	"path/filepath"
	"strings"

	"github.com/anggasct/aura/internal/platform/configkit"
)

func ValidateTools(toolsConfig *Tools) error {
	if toolsConfig == nil {
		return nil
	}
	if strings.TrimSpace(toolsConfig.Workspace) == "" {
		return configkit.Invalid("tools.workspace", "must not be empty")
	}
	if !filepath.IsAbs(toolsConfig.Workspace) {
		return configkit.Invalid("tools.workspace", "must be an absolute path")
	}
	if toolsConfig.MaxInlineResultBytes <= 0 {
		return configkit.Invalid("tools.max_inline_result_bytes", "must be positive")
	}
	if err := configkit.Under("tools.exec", toolsConfig.Exec.Validate()); err != nil {
		return err
	}
	if err := configkit.Under("tools.fetch", toolsConfig.Fetch.Validate()); err != nil {
		return err
	}
	if err := configkit.Under("tools.github", toolsConfig.GitHub.Validate()); err != nil {
		return err
	}
	return configkit.Under("tools.web_search", toolsConfig.WebSearch.Validate())
}
