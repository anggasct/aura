package config

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func writeSkillsConfig(t *testing.T, content string) string {
	t.Helper()
	return writeTempConfig(t, content)
}

func TestLoad_SkillsDefaults(t *testing.T) {
	path := writeSkillsConfig(t, "version: 1\nvision: {}\ntools:\n  workspace: /srv/aura/workspace\nskills:\n  roots: [/srv/aura/skills]\nprofile:\n  prompt_version: v1\nchildren:\n  recovery: interrupt\ncontext:\n  recent_complete_turns: 5\n")
	result, err := LoadWithOptions(path, execLinuxOptions(t))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := result.Config.Skills
	if got == nil {
		t.Fatal("Skills = nil")
	}
	if !got.Enabled || !got.AutoSelect {
		t.Errorf("skills = %+v", got)
	}
	if got.MaxIndexedSkills != 256 || got.MaxInstructionTokens != 5000 || got.MaxSessionContextTokens != 15000 || got.MaxResourceBytes != 8388608 {
		t.Errorf("skills = %+v", got)
	}
	if got.QuarantineRetention != Duration(720*time.Hour) {
		t.Errorf("retention = %v", got.QuarantineRetention)
	}
}

func TestLoad_SkillsValid(t *testing.T) {
	path := writeSkillsConfig(t, `version: 1
vision: {}
tools:
  workspace: /srv/aura/workspace
skills:
  enabled: true
  roots: [/srv/aura/skills]
  auto_select: false
  max_indexed_skills: 10
  max_instruction_tokens: 1000
  max_resource_bytes: 1024
  quarantine_retention: 48h
context:
  recent_complete_turns: 5
profile:
  prompt_version: v1
children:
  recovery: interrupt
`)
	result, err := LoadWithOptions(path, execLinuxOptions(t))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := result.Config.Skills
	if got.AutoSelect || got.MaxIndexedSkills != 10 || got.QuarantineRetention != Duration(48*time.Hour) {
		t.Errorf("skills = %+v", got)
	}
}

func TestLoad_SkillsEnvOverride(t *testing.T) {
	t.Setenv("AURA_SKILLS_MAX_INDEXED_SKILLS", "32")
	path := writeSkillsConfig(t, "version: 1\nvision: {}\ntools:\n  workspace: /srv/aura/workspace\nskills:\n  roots: [/srv/aura/skills]\nprofile:\n  prompt_version: v1\nchildren:\n  recovery: interrupt\ncontext:\n  recent_complete_turns: 5\n")
	result, err := LoadWithOptions(path, execLinuxOptions(t))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if result.Config.Skills.MaxIndexedSkills != 32 {
		t.Errorf("max_indexed_skills = %d", result.Config.Skills.MaxIndexedSkills)
	}
}

func TestLoad_SkillsInvalid(t *testing.T) {
	cases := map[string]string{
		"missing section":     "version: 1\nvision: {}\ntools:\n  workspace: /srv/aura/workspace\n",
		"empty roots":         "version: 1\nvision: {}\ntools:\n  workspace: /srv/aura/workspace\nskills:\n  roots: []\n",
		"relative root":       "version: 1\nvision: {}\ntools:\n  workspace: /srv/aura/workspace\nskills:\n  roots: [skills]\n",
		"unclean root":        "version: 1\nvision: {}\ntools:\n  workspace: /srv/aura/workspace\nskills:\n  roots: [/srv/aura/skills/]\n",
		"zero indexed":        "version: 1\nvision: {}\ntools:\n  workspace: /srv/aura/workspace\nskills:\n  roots: [/s]\n  max_indexed_skills: 0\n",
		"huge indexed":        "version: 1\nvision: {}\ntools:\n  workspace: /srv/aura/workspace\nskills:\n  roots: [/s]\n  max_indexed_skills: 5000\n",
		"zero tokens":         "version: 1\nvision: {}\ntools:\n  workspace: /srv/aura/workspace\nskills:\n  roots: [/s]\n  max_instruction_tokens: 0\n",
		"zero resource bytes": "version: 1\nvision: {}\ntools:\n  workspace: /srv/aura/workspace\nskills:\n  roots: [/s]\n  max_resource_bytes: 0\n",
		"zero retention":      "version: 1\nvision: {}\ntools:\n  workspace: /srv/aura/workspace\nskills:\n  roots: [/s]\n  quarantine_retention: 0s\n",
		"bool as string":      "version: 1\nvision: {}\ntools:\n  workspace: /srv/aura/workspace\nskills:\n  roots: [/s]\n  enabled: \"yes\"\n",
		"roots as string":     "version: 1\nvision: {}\ntools:\n  workspace: /srv/aura/workspace\nskills:\n  roots: /s\n",
		"unknown key":         "version: 1\nvision: {}\ntools:\n  workspace: /srv/aura/workspace\nskills:\n  roots: [/s]\n  teleport: true\n",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := LoadWithOptions(writeSkillsConfig(t, content), execLinuxOptions(t))
			if err == nil {
				t.Errorf("expected error")
			} else if !strings.Contains(err.Error(), "config_invalid") && !strings.Contains(err.Error(), "skills") {
				t.Errorf("err = %v", err)
			}
		})
	}
}

const validSkillsBase = "version: 1\nvision: {}\ntools:\n  workspace: /srv/aura/workspace\nprofile:\n  prompt_version: v1\nchildren:\n  recovery: interrupt\ncontext:\n  recent_complete_turns: 5\nskills:\n  roots: [/srv/aura/skills]\n"

func TestLoad_SkillsSessionContextBound(t *testing.T) {
	result, err := LoadWithOptions(writeSkillsConfig(t, validSkillsBase+"  max_instruction_tokens: 2000\n  max_session_context_tokens: 2000\n"), execLinuxOptions(t))
	if err != nil {
		t.Fatalf("equal bounds rejected: %v", err)
	}
	if result.Config.Skills.MaxSessionContextTokens != 2000 {
		t.Errorf("max_session_context_tokens = %d", result.Config.Skills.MaxSessionContextTokens)
	}
	t.Setenv("AURA_SKILLS_MAX_SESSION_CONTEXT_TOKENS", "24000")
	result, err = LoadWithOptions(writeSkillsConfig(t, validSkillsBase), execLinuxOptions(t))
	if err != nil {
		t.Fatalf("env override: %v", err)
	}
	if result.Config.Skills.MaxSessionContextTokens != 24000 {
		t.Errorf("env max_session_context_tokens = %d", result.Config.Skills.MaxSessionContextTokens)
	}
}

func TestLoad_SkillsSessionContextBoundInvalid(t *testing.T) {
	cases := map[string]string{
		"below instruction bound": "  max_instruction_tokens: 6000\n  max_session_context_tokens: 5999\n",
		"below default":           "  max_session_context_tokens: 4999\n",
		"zero":                    "  max_session_context_tokens: 0\n",
		"string":                  "  max_session_context_tokens: \"lots\"\n",
	}
	for name, extra := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := LoadWithOptions(writeSkillsConfig(t, validSkillsBase+extra), execLinuxOptions(t))
			if err == nil || !strings.Contains(err.Error(), "max_session_context_tokens") {
				t.Fatalf("err = %v, want a max_session_context_tokens rejection", err)
			}
		})
	}
	_, err := LoadWithOptions(writeSkillsConfig(t, validSkillsBase+cases["below instruction bound"]), execLinuxOptions(t))
	var cfgErr *Error
	if !errors.As(err, &cfgErr) || cfgErr.Code != ErrorCodeConfigInvalid {
		t.Fatalf("err = %v, want config_invalid", err)
	}
}
