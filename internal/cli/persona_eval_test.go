package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/store"
)

func writePersonaEvalConfig(t *testing.T, instructions, publicTools string) string {
	t.Helper()
	root := t.TempDir()
	path := filepath.Join(root, "config.yaml")
	content := `version: 1
storage:
  path: ` + root + `
tools:
  workspace: ` + root + `
agents:
  definitions:
    - id: lobby
      description: front desk
      instructions: "` + instructions + `"
      audience: public
      tools: [read_file, web_search, handoff_to_owner]
      principal_tools: [read_file, handoff_to_owner]
      public_tools: [` + publicTools + `]
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	seedConfiguredStorage(t, path)
	return path
}

func writeEvalCases(t *testing.T, configPath, persona, content string) {
	t.Helper()
	dir := filepath.Join(filepath.Dir(configPath), "evals", persona)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cases.yaml"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func evalGateFor(t *testing.T, configPath string) *personaEvalGate {
	t.Helper()
	loaded, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := buildAgentRegistry(loaded.Config)
	if err != nil {
		t.Fatal(err)
	}
	return &personaEvalGate{registry: registry, results: store.NewPersonaEvalStore(openAccessDB(t, configPath))}
}

func TestPersonaEvalRecordsResultsBoundToTheDigest(t *testing.T) {
	cfg := writePersonaEvalConfig(t, "You help visitors.", "web_search, handoff_to_owner")
	writeEvalCases(t, cfg, "lobby", "cases:\n  - name: public tools\n    kind: tool_exposure\n    class: public\n    want: [web_search, handoff_to_owner]\n")
	if passed, _ := evalGateFor(t, cfg).Evaluated(t.Context(), "lobby"); passed {
		t.Fatal("persona admitted before any evaluation")
	}
	out, err := runAccessCommand(t, newPersonaCmd, cfg, "eval", "lobby")
	if err != nil || !strings.Contains(out, "result: pass") {
		t.Fatalf("eval = %q, %v", out, err)
	}
	if passed, err := evalGateFor(t, cfg).Evaluated(t.Context(), "lobby"); err != nil || !passed {
		t.Fatalf("gate after a passing run = %v, %v", passed, err)
	}
	out, err = runAccessCommand(t, newPersonaCmd, cfg, "eval-results", "lobby")
	if err != nil || !strings.Contains(out, "policy\tpass") || !strings.Contains(out, "\ttrue") {
		t.Fatalf("eval-results = %q, %v", out, err)
	}
	for name, changed := range map[string]string{
		"soul":         writePersonaEvalConfigAt(t, cfg, "You help visitors politely.", "web_search, handoff_to_owner"),
		"public tools": writePersonaEvalConfigAt(t, cfg, "You help visitors.", "web_search"),
	} {
		if passed, _ := evalGateFor(t, changed).Evaluated(t.Context(), "lobby"); passed {
			t.Fatalf("changing the %s kept the old evaluation", name)
		}
	}
	writeEvalCases(t, cfg, "lobby", "cases:\n  - name: wrong tools\n    kind: tool_exposure\n    class: public\n    want: [read_file]\n")
	out, err = runAccessCommand(t, newPersonaCmd, cfg, "eval", "lobby")
	if err == nil || !strings.Contains(out, "failed: wrong tools") || !strings.Contains(out, "result: fail") {
		t.Fatalf("failing eval = %q, %v", out, err)
	}
	if passed, _ := evalGateFor(t, cfg).Evaluated(t.Context(), "lobby"); passed {
		t.Fatal("the latest failing run kept the persona admitted")
	}
	if _, err := runAccessCommand(t, newPersonaCmd, cfg, "eval", "nobody"); err == nil {
		t.Fatal("eval of an unknown persona succeeded")
	}
}

func writePersonaEvalConfigAt(t *testing.T, original, instructions, publicTools string) string {
	t.Helper()
	data, err := os.ReadFile(original)
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	start := strings.Index(content, "      instructions: ")
	end := strings.Index(content[start:], "\n") + start
	content = content[:start] + `      instructions: "` + instructions + `"` + content[end:]
	start = strings.Index(content, "      public_tools: ")
	end = strings.Index(content[start:], "\n") + start
	content = content[:start] + "      public_tools: [" + publicTools + "]" + content[end:]
	path := filepath.Join(filepath.Dir(original), "changed-"+strings.ReplaceAll(publicTools+instructions, " ", "-")[:12]+".yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
