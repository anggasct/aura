package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/workflow"
)

func writeWorkflowFixtures(t *testing.T, definition string) *globalFlags {
	t.Helper()
	dir := t.TempDir()
	defsDir := filepath.Join(dir, "workflows")
	if err := os.MkdirAll(defsDir, 0o700); err != nil {
		t.Fatalf("mkdir defs: %v", err)
	}
	if definition != "" {
		if err := os.WriteFile(filepath.Join(defsDir, "demo.yaml"), []byte(definition), 0o600); err != nil {
			t.Fatalf("write definition: %v", err)
		}
	}
	cfg := `version: 1
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
storage:
  path: ` + dir + `
workflows:
  definitions_dir: ` + defsDir + `
`
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, []byte(cfg), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return &globalFlags{configPath: configPath}
}

const validWorkflowYAML = `id: demo
version: 1
goal: Demonstrate the CLI contract
source: defined
steps:
  - id: approve
    executor: { kind: approval }
    timeout: 1m
`

func runWorkflowCommand(t *testing.T, gf *globalFlags, args ...string) (string, error) {
	t.Helper()
	cmd := newWorkflowCmd(gf)
	out := &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetErr(out)
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(t.Context())
	return out.String(), err
}

func TestWorkflowValidateAcceptsValidFile(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "demo.yaml")
	if err := os.WriteFile(file, []byte(validWorkflowYAML), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	gf := writeWorkflowFixtures(t, "")
	out, err := runWorkflowCommand(t, gf, "validate", file)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if !strings.Contains(out, "valid: demo") {
		t.Fatalf("output = %q, want the valid marker", out)
	}
}

func TestWorkflowValidateRejectsInvalidFile(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "broken.yaml")
	if err := os.WriteFile(file, []byte("id: broken\nversion: 1\ngoal: g\nsteps:\n  - id: a\n    executor: { kind: shell }\n    timeout: 1m\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	gf := writeWorkflowFixtures(t, "")
	if _, err := runWorkflowCommand(t, gf, "validate", file); err == nil {
		t.Fatal("validate accepted an invalid executor kind")
	} else if code, ok := workflow.CodeOf(err); !ok || code != workflow.ErrorCodeExecutorInvalid {
		t.Fatalf("error = %v, want workflow_executor_invalid", err)
	}
}

func TestWorkflowStartUnknownDefinitionFails(t *testing.T) {
	gf := writeWorkflowFixtures(t, validWorkflowYAML)
	_, err := runWorkflowCommand(t, gf, "start", "ghost")
	if err == nil {
		t.Fatal("start accepted an unknown definition")
	}
	if code, ok := workflow.CodeOf(err); !ok || code != workflow.ErrorCodeDefinitionNotFound {
		t.Fatalf("error = %v, want workflow_definition_not_found", err)
	}
}

func TestWorkflowRunsAndInspectUnknownRunFails(t *testing.T) {
	gf := writeWorkflowFixtures(t, validWorkflowYAML)
	if _, err := runWorkflowCommand(t, gf, "inspect", "run-missing"); err == nil {
		t.Fatal("inspect accepted an unknown run")
	} else if code, ok := workflow.CodeOf(err); !ok || code != workflow.ErrorCodeRunNotFound {
		t.Fatalf("inspect error = %v, want workflow_run_not_found", err)
	}
	if _, err := runWorkflowCommand(t, gf, "cancel", "run-missing"); err == nil {
		t.Fatal("cancel accepted an unknown run")
	}
	if _, err := runWorkflowCommand(t, gf, "runs", "--status", "queued"); err != nil {
		t.Fatalf("runs listing failed: %v", err)
	}
}
