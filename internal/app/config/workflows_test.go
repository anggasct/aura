package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	legacyconfig "github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/modules/workflow"
)

func TestGeneratedWorkflowBudgets(t *testing.T) {
	if got := writeConfigYAML(t, "").Config.Workflows.Generated; got != (workflow.GeneratedConfig{MaxSteps: 20, MaxTokens: 200000}) {
		t.Fatalf("defaults = %+v", got)
	}
	if got := writeConfigYAML(t, "workflows:\n  generated:\n    max_steps: 5\n").Config.Workflows.Generated; got != (workflow.GeneratedConfig{MaxSteps: 5, MaxTokens: 200000}) {
		t.Fatalf("partial override = %+v", got)
	}
	for label, tc := range map[string]struct {
		body string
		code legacyconfig.ErrorCode
	}{
		"zero steps":     {"workflows:\n  generated:\n    max_steps: 0\n", invalidCode},
		"negative token": {"workflows:\n  generated:\n    max_tokens: -1\n", invalidCode},
		"text steps":     {"workflows:\n  generated:\n    max_steps: many\n", invalidCode},
		"unknown key":    {"workflows:\n  generated:\n    max_children: 2\n", unknownKeyCode},
		"not a mapping":  {"workflows:\n  generated: 20\n", invalidCode},
	} {
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte("version: 1\n"+tc.body), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := Load(path)
		if code, ok := codeOf(err); !ok || code != tc.code || !strings.Contains(err.Error(), "workflows.generated") {
			t.Errorf("%s: err = %v, want %s naming workflows.generated", label, err, tc.code)
		}
	}
}
