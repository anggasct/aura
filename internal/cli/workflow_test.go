package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/config"
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

const noTimeoutWorkflowYAML = `id: notime
version: 1
goal: The configured default step timeout applies
source: defined
steps:
  - id: gate
    executor: { kind: approval }
`

const brokenAgentConfig = `version: 1
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
agents:
  definitions:
    - id: broken
      description: broken
      instructions: do nothing
      tools: [no_such_tool]
storage:
  path: DIR
workflows:
  definitions_dir: DEFS
`

func writeBrokenAgentFixtures(t *testing.T, definition string) *globalFlags {
	t.Helper()
	gf := writeWorkflowFixtures(t, "")
	cfgPath := gf.configPath
	dir := filepath.Dir(cfgPath)
	defsDir := filepath.Join(dir, "workflows")
	cfg := strings.ReplaceAll(brokenAgentConfig, "DIR", dir)
	cfg = strings.ReplaceAll(cfg, "DEFS", defsDir)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if definition != "" {
		if err := os.WriteFile(filepath.Join(defsDir, "broken.yaml"), []byte(definition), 0o600); err != nil {
			t.Fatalf("write definition: %v", err)
		}
	}
	return gf
}

func TestWorkflowValidateFailsClosedWhenRegistryBuildFails(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "demo.yaml")
	if err := os.WriteFile(file, []byte(validWorkflowYAML), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	gf := writeBrokenAgentFixtures(t, validWorkflowYAML)
	if _, err := runWorkflowCommand(t, gf, "validate", file); err == nil {
		t.Fatal("validate tolerated an agent registry build failure")
	}
	if _, err := runWorkflowCommand(t, gf, "definitions"); err == nil {
		t.Fatal("definitions tolerated an agent registry build failure")
	}
}

func TestWorkflowValidateAcceptsOmittedTimeoutWithConfiguredDefault(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "notime.yaml")
	if err := os.WriteFile(file, []byte(noTimeoutWorkflowYAML), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	gf := writeWorkflowFixtures(t, "")
	out, err := runWorkflowCommand(t, gf, "validate", file)
	if err != nil {
		t.Fatalf("validate rejected an omitted timeout: %v", err)
	}
	if !strings.Contains(out, "valid: notime") {
		t.Fatalf("output = %q, want the valid marker", out)
	}
}

func TestWorkflowCancelPreservesTerminalRun(t *testing.T) {
	gf := writeWorkflowFixtures(t, validWorkflowYAML)
	ctx := t.Context()
	result, err := config.Load(gf.configPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	dir := filepath.Dir(gf.configPath)
	spec, err := workflow.LoadSpecFile(filepath.Join(dir, "workflows", "demo.yaml"))
	if err != nil {
		t.Fatalf("load fixture spec: %v", err)
	}
	db, err := openStorage(ctx, result.Config)
	if err != nil {
		t.Fatalf("open storage: %v", err)
	}
	disk := workflow.NewStore(db)
	if err := disk.SaveDefinition(ctx, spec); err != nil {
		t.Fatalf("save definition: %v", err)
	}
	summary, err := disk.CreateRun(ctx, spec, nil)
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	if err := disk.SetRunStatus(ctx, summary.ID, workflow.RunSucceeded); err != nil {
		t.Fatalf("succeed run: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	out, err := runWorkflowCommand(t, gf, "cancel", summary.ID)
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if !strings.Contains(out, "status: "+workflow.RunSucceeded) {
		t.Fatalf("cancel output = %q, want the preserved terminal status", out)
	}
	db, err = openStorage(ctx, result.Config)
	if err != nil {
		t.Fatalf("reopen storage: %v", err)
	}
	defer func() { _ = db.Close() }()
	run, err := workflow.NewStore(db).Run(ctx, summary.ID)
	if err != nil {
		t.Fatalf("read run: %v", err)
	}
	if run.Status != workflow.RunSucceeded {
		t.Fatalf("run status = %s, want the terminal succeeded state preserved", run.Status)
	}
}

const outputWorkflowYAML = `id: outdemo
version: 1
goal: Inspect renders step outputs
source: defined
steps:
  - id: emit
    executor: { kind: approval }
    timeout: 1m
  - id: report
    executor: { kind: approval }
    timeout: 1m
`

func TestWorkflowInspectRendersStepOutputs(t *testing.T) {
	gf := writeWorkflowFixtures(t, outputWorkflowYAML)
	ctx := t.Context()
	result, err := config.Load(gf.configPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	dir := filepath.Dir(gf.configPath)
	spec, err := workflow.LoadSpecFile(filepath.Join(dir, "workflows", "demo.yaml"))
	if err != nil {
		t.Fatalf("load fixture spec: %v", err)
	}
	db, err := openStorage(ctx, result.Config)
	if err != nil {
		t.Fatalf("open storage: %v", err)
	}
	disk := workflow.NewStore(db)
	if err := disk.SaveDefinition(ctx, spec); err != nil {
		t.Fatalf("save definition: %v", err)
	}
	summary, err := disk.CreateRun(ctx, spec, nil)
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := db.Exec(`UPDATE workflow_step_run SET status = ?, attempt = 1, ended_at = ?, output_json = ? WHERE run_id = ? AND step_id = ?`,
		workflow.StepSucceeded, now, `{"value":42}`, summary.ID, "emit"); err != nil {
		t.Fatalf("seed inline output: %v", err)
	}
	if _, err := db.Exec(`UPDATE workflow_step_run SET status = ?, attempt = 1, ended_at = ?, output_json = ?, output_artifact_digest = ? WHERE run_id = ? AND step_id = ?`,
		workflow.StepSucceeded, now, `{"artifact_digest":"sha256-inspect"}`, "sha256-inspect", summary.ID, "report"); err != nil {
		t.Fatalf("seed artifact output: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	out, err := runWorkflowCommand(t, gf, "inspect", summary.ID)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if !strings.Contains(out, "output: {\"value\":42}") {
		t.Fatalf("inspect output = %q, want the inline step output", out)
	}
	if !strings.Contains(out, "artifact: sha256-inspect") {
		t.Fatalf("inspect output = %q, want the artifact digest reference", out)
	}
}

func TestNewWorkflowToolRunnerPropagatesConstructionErrors(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("XDG_DATA_HOME", "")
	if _, err := newWorkflowToolRunner(&config.Config{Tools: &config.Tools{}}, nil); err == nil {
		t.Fatal("tool runner construction swallowed the storage path failure")
	} else if !strings.Contains(err.Error(), "cannot resolve data directory") {
		t.Fatalf("error = %v, want the data directory cause", err)
	}
	cfg := &config.Config{
		Tools:   &config.Tools{Workspace: t.TempDir()},
		Storage: config.Storage{Path: t.TempDir()},
	}
	if _, err := newWorkflowToolRunner(cfg, nil); err == nil {
		t.Fatal("tool runner construction swallowed the executor failure")
	} else if !strings.Contains(err.Error(), "storage database is required") {
		t.Fatalf("error = %v, want the executor cause", err)
	}
}

func TestWorkflowStartFailsLoudlyWhenDataRootUnresolvable(t *testing.T) {
	gf := writeWorkflowFixtures(t, validWorkflowYAML)
	content, err := os.ReadFile(gf.configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	trimmed := strings.ReplaceAll(string(content), "storage:\n  path: "+filepath.Dir(gf.configPath)+"\n", "")
	if trimmed == string(content) {
		t.Fatal("fixture rewrite removed no storage path")
	}
	if err := os.WriteFile(gf.configPath, []byte(trimmed), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv("HOME", "")
	t.Setenv("XDG_DATA_HOME", "")
	if _, err := runWorkflowCommand(t, gf, "start", "demo"); err == nil {
		t.Fatal("start tolerated an unresolvable data root")
	} else if !strings.Contains(err.Error(), "cannot resolve data directory") {
		t.Fatalf("error = %v, want the underlying cause", err)
	}
}
