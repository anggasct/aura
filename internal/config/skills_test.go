package config

import (
	"strings"
	"testing"
	"time"
)

func writeSkillsConfig(t *testing.T, content string) string {
	t.Helper()
	return writeTempConfig(t, content)
}

func TestLoad_SkillsDefaults(t *testing.T) {
	path := writeSkillsConfig(t, "version: 1\ntools:\n  workspace: /srv/aura/workspace\nskills:\n  roots: [/srv/aura/skills]\n")
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
	if got.MaxIndexedSkills != 256 || got.MaxInstructionTokens != 5000 || got.MaxResourceBytes != 8388608 {
		t.Errorf("skills = %+v", got)
	}
	if got.QuarantineRetention != Duration(720*time.Hour) {
		t.Errorf("retention = %v", got.QuarantineRetention)
	}
}

func TestLoad_SkillsValid(t *testing.T) {
	path := writeSkillsConfig(t, `version: 1
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
	path := writeSkillsConfig(t, "version: 1\ntools:\n  workspace: /srv/aura/workspace\nskills:\n  roots: [/srv/aura/skills]\n")
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
		"missing section":     "version: 1\ntools:\n  workspace: /srv/aura/workspace\n",
		"empty roots":         "version: 1\ntools:\n  workspace: /srv/aura/workspace\nskills:\n  roots: []\n",
		"relative root":       "version: 1\ntools:\n  workspace: /srv/aura/workspace\nskills:\n  roots: [skills]\n",
		"unclean root":        "version: 1\ntools:\n  workspace: /srv/aura/workspace\nskills:\n  roots: [/srv/aura/skills/]\n",
		"zero indexed":        "version: 1\ntools:\n  workspace: /srv/aura/workspace\nskills:\n  roots: [/s]\n  max_indexed_skills: 0\n",
		"huge indexed":        "version: 1\ntools:\n  workspace: /srv/aura/workspace\nskills:\n  roots: [/s]\n  max_indexed_skills: 5000\n",
		"zero tokens":         "version: 1\ntools:\n  workspace: /srv/aura/workspace\nskills:\n  roots: [/s]\n  max_instruction_tokens: 0\n",
		"zero resource bytes": "version: 1\ntools:\n  workspace: /srv/aura/workspace\nskills:\n  roots: [/s]\n  max_resource_bytes: 0\n",
		"zero retention":      "version: 1\ntools:\n  workspace: /srv/aura/workspace\nskills:\n  roots: [/s]\n  quarantine_retention: 0s\n",
		"bool as string":      "version: 1\ntools:\n  workspace: /srv/aura/workspace\nskills:\n  roots: [/s]\n  enabled: \"yes\"\n",
		"roots as string":     "version: 1\ntools:\n  workspace: /srv/aura/workspace\nskills:\n  roots: /s\n",
		"unknown key":         "version: 1\ntools:\n  workspace: /srv/aura/workspace\nskills:\n  roots: [/s]\n  teleport: true\n",
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
