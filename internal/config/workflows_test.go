package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGeneratedWorkflowBudgets(t *testing.T) {
	if got := writeConfigYAML(t, "").Config.Workflows.Generated; got != (GeneratedWorkflows{MaxSteps: 20, MaxTokens: 200000}) {
		t.Fatalf("defaults = %+v", got)
	}
	if got := writeConfigYAML(t, "workflows:\n  generated:\n    max_steps: 5\n").Config.Workflows.Generated; got != (GeneratedWorkflows{MaxSteps: 5, MaxTokens: 200000}) {
		t.Fatalf("partial override = %+v", got)
	}
	for label, body := range map[string]string{
		"zero steps":     "workflows:\n  generated:\n    max_steps: 0\n",
		"negative token": "workflows:\n  generated:\n    max_tokens: -1\n",
		"text steps":     "workflows:\n  generated:\n    max_steps: many\n",
		"unknown key":    "workflows:\n  generated:\n    max_children: 2\n",
		"not a mapping":  "workflows:\n  generated: 20\n",
	} {
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte("version: 1\n"+body), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := Load(path)
		if code, ok := CodeOf(err); !ok || code != ErrorCodeConfigInvalid || !strings.Contains(err.Error(), "workflows.generated") {
			t.Errorf("%s: err = %v, want config_invalid naming workflows.generated", label, err)
		}
	}
}
