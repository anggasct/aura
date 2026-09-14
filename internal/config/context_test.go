package config

import (
	"strings"
	"testing"
)

func writeContextConfig(t *testing.T, content string) string {
	t.Helper()
	return writeTempConfig(t, content)
}

func contextBase() string {
	return "version: 1\ntools:\n  workspace: /srv/aura/workspace\nskills:\n  roots: [/srv/aura/skills]\ncontext:\n"
}

func TestLoad_ContextDefaults(t *testing.T) {
	path := writeContextConfig(t, contextBase()+"  recent_complete_turns: 5\n")
	result, err := LoadWithOptions(path, execLinuxOptions(t))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := result.Config.Context
	if got == nil {
		t.Fatal("Context = nil")
	}
	if !got.Enabled || got.RecentCompleteTurns != 5 {
		t.Errorf("context = %+v", got)
	}
	if got.SafetyMarginTokens != 1024 || got.ConservativeEstimatorRatio != 0.80 || got.HighWaterRatio != 0.90 {
		t.Errorf("context = %+v", got)
	}
	if got.MaxToolExcerptBytes != 2048 {
		t.Errorf("context = %+v", got)
	}
	summary := got.Summary
	if summary.Task != "compress" || summary.MaxSourceTokens != 32768 || summary.MaxOutputTokens != 2048 || summary.PromptVersion != "v1" {
		t.Errorf("summary = %+v", summary)
	}
}

func TestLoad_ContextValid(t *testing.T) {
	path := writeContextConfig(t, contextBase()+`  enabled: false
  recent_complete_turns: 3
  safety_margin_tokens: 512
  conservative_estimator_ratio: 0.5
  high_water_ratio: 0.75
  max_tool_excerpt_bytes: 1024
  summary:
    task: compression
    max_source_tokens: 4096
    max_output_tokens: 512
    prompt_version: v2
`)
	result, err := LoadWithOptions(path, execLinuxOptions(t))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := result.Config.Context
	if got.Enabled || got.RecentCompleteTurns != 3 || got.SafetyMarginTokens != 512 {
		t.Errorf("context = %+v", got)
	}
	if got.ConservativeEstimatorRatio != 0.5 || got.HighWaterRatio != 0.75 || got.MaxToolExcerptBytes != 1024 {
		t.Errorf("context = %+v", got)
	}
	if got.Summary.Task != "compression" || got.Summary.PromptVersion != "v2" {
		t.Errorf("summary = %+v", got.Summary)
	}
}

func TestLoad_ContextEnvOverride(t *testing.T) {
	t.Setenv("AURA_CONTEXT_SAFETY_MARGIN_TOKENS", "77")
	path := writeContextConfig(t, contextBase()+"  recent_complete_turns: 5\n")
	result, err := LoadWithOptions(path, execLinuxOptions(t))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if result.Config.Context.SafetyMarginTokens != 77 {
		t.Errorf("safety_margin_tokens = %d", result.Config.Context.SafetyMarginTokens)
	}
}

func TestLoad_ContextInvalid(t *testing.T) {
	cases := map[string]string{
		"missing section":               "version: 1\ntools:\n  workspace: /srv/aura/workspace\nskills:\n  roots: [/srv/aura/skills]\n",
		"zero turns":                    contextBase() + "  recent_complete_turns: 0\n",
		"zero margin":                   contextBase() + "  safety_margin_tokens: 0\n",
		"ratio zero":                    contextBase() + "  conservative_estimator_ratio: 0.0\n",
		"ratio one":                     contextBase() + "  high_water_ratio: 1.0\n",
		"conservative above high water": contextBase() + "  conservative_estimator_ratio: 0.95\n",
		"zero excerpt":                  contextBase() + "  max_tool_excerpt_bytes: 0\n",
		"empty task":                    contextBase() + "  summary:\n    task: \"\"\n",
		"zero source tokens":            contextBase() + "  summary:\n    max_source_tokens: 0\n",
		"zero output tokens":            contextBase() + "  summary:\n    max_output_tokens: 0\n",
		"empty prompt version":          contextBase() + "  summary:\n    prompt_version: \"\"\n",
		"enabled as string":             contextBase() + "  enabled: \"yes\"\n",
		"turns as string":               contextBase() + "  recent_complete_turns: \"5\"\n",
		"ratio as string":               contextBase() + "  conservative_estimator_ratio: \"0.5\"\n",
		"unknown key":                   contextBase() + "  teleport: true\n",
		"unknown summary key":           contextBase() + "  summary:\n    teleport: true\n",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := LoadWithOptions(writeContextConfig(t, content), execLinuxOptions(t))
			if err == nil {
				t.Errorf("expected error")
			} else if !strings.Contains(err.Error(), "config_invalid") && !strings.Contains(err.Error(), "context") {
				t.Errorf("err = %v", err)
			}
		})
	}
}
