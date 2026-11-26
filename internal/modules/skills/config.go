package skills

import (
	"errors"
	"path/filepath"
	"strings"
	"time"

	"github.com/anggasct/aura/internal/platform/configkit"
)

type Config struct {
	Enabled                 bool               `json:"enabled"`
	Roots                   []string           `json:"roots"`
	AutoSelect              bool               `json:"auto_select"`
	MaxIndexedSkills        int                `json:"max_indexed_skills"`
	MaxInstructionTokens    int                `json:"max_instruction_tokens"`
	MaxSessionContextTokens int                `json:"max_session_context_tokens"`
	MaxResourceBytes        int64              `json:"max_resource_bytes"`
	QuarantineRetention     configkit.Duration `json:"quarantine_retention"`
}

func Default() Config {
	return Config{
		Enabled:                 true,
		Roots:                   []string{"/srv/aura/skills"},
		AutoSelect:              true,
		MaxIndexedSkills:        256,
		MaxInstructionTokens:    5000,
		MaxSessionContextTokens: 15000,
		MaxResourceBytes:        8388608,
		QuarantineRetention:     configkit.Duration(720 * time.Hour),
	}
}

func (c *Config) Validate() error {
	if c == nil {
		return nil
	}
	var problems []error
	if len(c.Roots) == 0 {
		problems = append(problems, configkit.Invalid("roots", "must list at least one directory"))
	}
	for _, root := range c.Roots {
		switch {
		case strings.TrimSpace(root) == "":
			problems = append(problems, configkit.Invalid("roots", "entries must not be empty"))
		case !filepath.IsAbs(root):
			problems = append(problems, configkit.Invalid("roots", "entries must be absolute paths"))
		case filepath.Clean(root) != root:
			problems = append(problems, configkit.Invalid("roots", "entries must be clean paths"))
		}
	}
	if c.MaxIndexedSkills <= 0 || c.MaxIndexedSkills > 4096 {
		problems = append(problems, configkit.Invalid("max_indexed_skills", "must be between 1 and 4096"))
	}
	if c.MaxInstructionTokens <= 0 || c.MaxInstructionTokens > 128000 {
		problems = append(problems, configkit.Invalid("max_instruction_tokens", "must be between 1 and 128000"))
	}
	if c.MaxSessionContextTokens <= 0 {
		problems = append(problems, configkit.Invalid("max_session_context_tokens", "must be positive"))
	} else if c.MaxSessionContextTokens < c.MaxInstructionTokens {
		problems = append(problems, configkit.Invalid("max_session_context_tokens", "must be at least skills.max_instruction_tokens"))
	}
	if c.MaxResourceBytes <= 0 || c.MaxResourceBytes > 1<<30 {
		problems = append(problems, configkit.Invalid("max_resource_bytes", "must be between 1 and 1073741824"))
	}
	if c.QuarantineRetention <= 0 {
		problems = append(problems, configkit.Invalid("quarantine_retention", "must be positive"))
	}
	return errors.Join(problems...)
}
