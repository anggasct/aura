package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	auraagent "github.com/anggasct/aura/internal/agent"
	"github.com/anggasct/aura/internal/config"
)

func writeAgentsConfig(t *testing.T, agentsSection string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	base := `version: 1
models:
  definitions:
    primary:
      protocol: openai_chat_compat
      model: test-model
      api_key_env: AURA_TEST_MODEL_KEY
      capabilities:
        streaming: true
        tools: true
        context_tokens: 200000
        tokenizer: anthropic
`
	if agentsSection != "" {
		base += agentsSection
	}
	if err := os.WriteFile(path, []byte(base), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func runAgentsCommand(t *testing.T, gf *globalFlags, args ...string) (string, error) {
	t.Helper()
	cmd := newAgentsCmd(gf)
	out := &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetErr(out)
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(t.Context())
	return out.String(), err
}

func TestAgentsListPrintsRegisteredDefinitions(t *testing.T) {
	gf := &globalFlags{configPath: writeAgentsConfig(t, "")}
	out, err := runAgentsCommand(t, gf, "list")
	if err != nil {
		t.Fatalf("agents list: %v", err)
	}
	for _, want := range []string{"main", "engineer", "reviewer", "researcher"} {
		if !strings.Contains(out, want) {
			t.Errorf("agents list output lacks %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "repository.write") || !strings.Contains(out, "web.search") {
		t.Errorf("agents list output lacks capabilities:\n%s", out)
	}
}

func TestAgentsShowPrintsOneDefinition(t *testing.T) {
	gf := &globalFlags{configPath: writeAgentsConfig(t, "")}
	out, err := runAgentsCommand(t, gf, "show", "engineer")
	if err != nil {
		t.Fatalf("agents show: %v", err)
	}
	for _, want := range []string{"ID: engineer", "Instructions:", "Model route: primary", "Capabilities: repository.read"} {
		if !strings.Contains(out, want) {
			t.Errorf("agents show output lacks %q:\n%s", want, out)
		}
	}
}

func TestAgentsShowUnknownIDFailsWithStableError(t *testing.T) {
	gf := &globalFlags{configPath: writeAgentsConfig(t, "")}
	_, err := runAgentsCommand(t, gf, "show", "nonexistent")
	if err == nil {
		t.Fatal("agents show accepted an unknown id")
	}
	code, ok := auraagent.CodeOf(err)
	if !ok || code != auraagent.ErrorCodeNotFound {
		t.Fatalf("error = %v, want agent_not_found", err)
	}
}

func TestAgentsListReflectsConfigOverride(t *testing.T) {
	agents := `
agents:
  definitions:
    - id: engineer
      instructions: "Custom engineer instructions"
      capabilities: [repository.read]
      model_route: primary
`
	gf := &globalFlags{configPath: writeAgentsConfig(t, agents)}
	out, err := runAgentsCommand(t, gf, "list")
	if err != nil {
		t.Fatalf("agents list: %v", err)
	}
	if strings.Count(out, "engineer\n") != 1 && !strings.Contains(out, "engineer\trepository.read") {
		t.Errorf("override did not replace the builtin entry:\n%s", out)
	}
	if strings.Contains(out, "shell.execute") {
		t.Errorf("overridden engineer still declares dropped capabilities:\n%s", out)
	}
}

func TestInvalidAgentConfigFailsCommandsAtStartup(t *testing.T) {
	agents := `
agents:
  definitions:
    - id: broken
      description: broken
      instructions: x
      capabilities: [kernel.root]
`
	gf := &globalFlags{configPath: writeAgentsConfig(t, agents)}
	if _, err := runAgentsCommand(t, gf, "list"); err == nil {
		t.Fatal("agents list accepted an unknown capability")
	}
	if _, err := config.Load(gf.configPath); err != nil {
		t.Logf("config.Load: %v", err)
	}
}
