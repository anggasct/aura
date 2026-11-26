package skills

import (
	"strings"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/configkit"
	"github.com/anggasct/aura/internal/platform/fault"
)

func TestSkillsConfigDefault(t *testing.T) {
	cfg := Default()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Default().Validate() = %v", err)
	}
	if !cfg.Enabled || !cfg.AutoSelect {
		t.Errorf("default flags = %+v", cfg)
	}
	if len(cfg.Roots) != 1 || cfg.Roots[0] != "/srv/aura/skills" {
		t.Errorf("default roots = %v", cfg.Roots)
	}
	if cfg.MaxIndexedSkills != 256 || cfg.MaxInstructionTokens != 5000 || cfg.MaxSessionContextTokens != 15000 || cfg.MaxResourceBytes != 8388608 {
		t.Errorf("default limits = %+v", cfg)
	}
	if cfg.QuarantineRetention != configkit.Duration(720*time.Hour) {
		t.Errorf("default retention = %v", cfg.QuarantineRetention)
	}
}

func TestSkillsConfigValidate(t *testing.T) {
	var nilCfg *Config
	if err := nilCfg.Validate(); err != nil {
		t.Fatalf("nil Validate() = %v", err)
	}

	validCases := map[string]func(*Config){
		"default":             func(*Config) {},
		"max indexed upper":   func(c *Config) { c.MaxIndexedSkills = 4096 },
		"max indexed lower":   func(c *Config) { c.MaxIndexedSkills = 1 },
		"max tokens upper":    func(c *Config) { c.MaxInstructionTokens = 128000; c.MaxSessionContextTokens = 128000 },
		"equal session bound": func(c *Config) { c.MaxInstructionTokens = 2000; c.MaxSessionContextTokens = 2000 },
		"max resource upper":  func(c *Config) { c.MaxResourceBytes = 1 << 30 },
	}
	for name, mutate := range validCases {
		t.Run("valid/"+name, func(t *testing.T) {
			cfg := Default()
			mutate(&cfg)
			if err := cfg.Validate(); err != nil {
				t.Fatalf("Validate() = %v, want nil", err)
			}
		})
	}

	invalidCases := []struct {
		name      string
		mutate    func(*Config)
		wantField string
		wantText  string
	}{
		{
			name:      "empty roots",
			mutate:    func(c *Config) { c.Roots = nil },
			wantField: "roots",
		},
		{
			name:      "blank root entry",
			mutate:    func(c *Config) { c.Roots = []string{"  "} },
			wantField: "roots",
		},
		{
			name:      "relative root",
			mutate:    func(c *Config) { c.Roots = []string{"skills"} },
			wantField: "roots",
		},
		{
			name:      "unclean root",
			mutate:    func(c *Config) { c.Roots = []string{"/srv/aura/skills/"} },
			wantField: "roots",
		},
		{
			name:      "zero indexed",
			mutate:    func(c *Config) { c.MaxIndexedSkills = 0 },
			wantField: "max_indexed_skills",
		},
		{
			name:      "indexed above max",
			mutate:    func(c *Config) { c.MaxIndexedSkills = 4097 },
			wantField: "max_indexed_skills",
		},
		{
			name:      "zero instruction tokens",
			mutate:    func(c *Config) { c.MaxInstructionTokens = 0 },
			wantField: "max_instruction_tokens",
		},
		{
			name:      "instruction tokens above max",
			mutate:    func(c *Config) { c.MaxInstructionTokens = 128001; c.MaxSessionContextTokens = 128001 },
			wantField: "max_instruction_tokens",
		},
		{
			name:      "zero session context tokens",
			mutate:    func(c *Config) { c.MaxSessionContextTokens = 0 },
			wantField: "max_session_context_tokens",
		},
		{
			name:      "session context below instruction tokens",
			mutate:    func(c *Config) { c.MaxInstructionTokens = 6000; c.MaxSessionContextTokens = 5999 },
			wantField: "max_session_context_tokens",
			wantText:  "must be at least skills.max_instruction_tokens",
		},
		{
			name:      "zero resource bytes",
			mutate:    func(c *Config) { c.MaxResourceBytes = 0 },
			wantField: "max_resource_bytes",
		},
		{
			name:      "resource bytes above max",
			mutate:    func(c *Config) { c.MaxResourceBytes = (1 << 30) + 1 },
			wantField: "max_resource_bytes",
		},
		{
			name:      "zero quarantine retention",
			mutate:    func(c *Config) { c.QuarantineRetention = 0 },
			wantField: "quarantine_retention",
		},
	}
	for _, tc := range invalidCases {
		t.Run("invalid/"+tc.name, func(t *testing.T) {
			cfg := Default()
			tc.mutate(&cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatal("Validate() = nil, want error")
			}
			if !fault.HasCode(err, configkit.ErrorCodeInvalid) {
				t.Errorf("err = %v, want %s", err, configkit.ErrorCodeInvalid)
			}
			if !strings.Contains(err.Error(), tc.wantField) {
				t.Errorf("err = %v, want field %q", err, tc.wantField)
			}
			if tc.wantText != "" && !strings.Contains(err.Error(), tc.wantText) {
				t.Errorf("err = %v, want substring %q", err, tc.wantText)
			}
		})
	}
}
