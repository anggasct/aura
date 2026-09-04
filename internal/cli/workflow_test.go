package cli

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/durable"
	"github.com/anggasct/aura/internal/store"
	toolsbuiltin "github.com/anggasct/aura/internal/tools/builtin"
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
	if _, err := db.ExecContext(ctx, `UPDATE workflow_step_run SET status = ?, attempt = 1, ended_at = ?, output_json = ? WHERE run_id = ? AND step_id = ?`,
		workflow.StepSucceeded, now, `{"value":42}`, summary.ID, "emit"); err != nil {
		t.Fatalf("seed inline output: %v", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE workflow_step_run SET status = ?, attempt = 1, ended_at = ?, output_json = ?, output_artifact_digest = ? WHERE run_id = ? AND step_id = ?`,
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
	if _, err := newWorkflowToolRunner(&config.Config{Tools: &config.Tools{}}, nil, nil); err == nil {
		t.Fatal("tool runner construction swallowed the storage path failure")
	} else if !strings.Contains(err.Error(), "cannot resolve data directory") {
		t.Fatalf("error = %v, want the data directory cause", err)
	}
	cfg := &config.Config{
		Tools:   &config.Tools{Workspace: t.TempDir()},
		Storage: config.Storage{Path: t.TempDir()},
	}
	if _, err := newWorkflowToolRunner(cfg, nil, nil); err == nil {
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

func newWorkflowToolRunnerFixture(t *testing.T) (workflow.ToolRunner, *sql.DB, string) {
	t.Helper()
	dir := t.TempDir()
	workspace := filepath.Join(dir, "workspace")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}
	cfg := config.Default()
	cfg.Tools.Workspace = workspace
	cfg.Storage.Path = dir
	ctx := t.Context()
	db, err := store.OpenDB(ctx, filepath.Join(dir, "aura.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.Migrate(ctx, db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	runner, err := newWorkflowToolRunner(&cfg, db, slog.Default())
	if err != nil {
		t.Fatalf("newWorkflowToolRunner: %v", err)
	}
	return runner, db, workspace
}

// TestWorkflowToolRunnerExecutesCoveredEffectfulTool proves the broker and
// approval-decider path unlock an effectful tool when the step carries real
// arguments: the tool receives the authored arguments, never a placeholder
// payload, and the effectful execution lands on disk.
func TestWorkflowToolRunnerExecutesCoveredEffectfulTool(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("builtin tool executor requires Linux")
	}
	runner, _, workspace := newWorkflowToolRunnerFixture(t)
	args := json.RawMessage(`{"path":"notes.txt","content":"authored by the workflow step"}`)
	output, err := runner.Invoke(t.Context(), "write_file", args)
	if err != nil {
		t.Fatalf("Invoke with real arguments: %v", err)
	}
	var result struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("decode tool result: %v (%s)", err, output)
	}
	content, err := os.ReadFile(filepath.Join(workspace, "notes.txt"))
	if err != nil {
		t.Fatalf("read authored file: %v", err)
	}
	if string(content) != "authored by the workflow step" {
		t.Fatalf("file content = %q, want the authored arguments, not a placeholder payload", content)
	}
}

// TestWorkflowToolRunnerRejectsEmptyArgumentsFailClosed proves a tool step
// without authored arguments never executes a canned payload: the adapter
// fails the step closed with the stable workflow_step_failed code and no
// file materializes.
func TestWorkflowToolRunnerRejectsEmptyArgumentsFailClosed(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("builtin tool executor requires Linux")
	}
	runner, _, workspace := newWorkflowToolRunnerFixture(t)
	for _, args := range []json.RawMessage{nil, json.RawMessage(`{}`), json.RawMessage(`null`)} {
		_, err := runner.Invoke(t.Context(), "write_file", args)
		if err == nil {
			t.Fatalf("Invoke accepted empty arguments %s", args)
		}
		if code, ok := workflow.CodeOf(err); !ok || code != workflow.ErrorCodeStepFailed {
			t.Fatalf("error = %v, want the stable workflow_step_failed code", err)
		}
	}
	if _, err := os.Stat(filepath.Join(workspace, ".workflow")); !os.IsNotExist(err) {
		t.Fatalf("placeholder payload file exists (err=%v): no canned payload may execute", err)
	}
}

// TestWorkflowToolRunnerRunsNoPlaceholderPayloadOverInterpreter proves the
// interpreter path with the shipped runner fails the tool step closed
// (workflow_step_failed) instead of executing a fabricated demo payload.
func TestWorkflowToolRunnerRunsNoPlaceholderPayloadOverInterpreter(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("builtin tool executor requires Linux")
	}
	runner, db, workspace := newWorkflowToolRunnerFixture(t)

	toolPtr := func(s string) *string { return &s }
	fake := durable.NewFake()
	disk := workflow.NewStore(db)
	interpreter := workflow.NewInterpreter(disk, fake, &workflow.Options{
		Tools:  runner,
		Logger: slog.Default(),
	})

	spec := &workflow.Spec{
		ID:      "covered-write-file",
		Goal:    "Execute covered effectful tool",
		Version: 1,
		Source:  workflow.SourceDefined,
		Steps: []workflow.StepSpec{
			{ID: "approve", Executor: workflow.ExecutorSpec{Kind: workflow.KindApproval}, Timeout: 5 * time.Second},
			{ID: "run_tool", DependsOn: []string{"approve"}, Executor: workflow.ExecutorSpec{Kind: workflow.KindTool, ToolID: toolPtr("write_file")}, Timeout: 5 * time.Second},
		},
	}
	deps := workflow.ValidationDeps{
		KnownTools:     toolsbuiltin.DefinitionNames(),
		EffectfulTools: toolsbuiltin.EffectfulToolNames(),
	}
	ctx := t.Context()
	if err := interpreter.Load(ctx, spec, deps); err != nil {
		t.Fatalf("Load: %v", err)
	}

	summary, err := interpreter.Start(ctx, spec.ID, nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		run, err := disk.Run(ctx, summary.ID)
		if err == nil && run.Status == workflow.RunSuspended {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if err := fake.Signal(ctx, durable.RunRef{Key: summary.DurableKey}, "approval.approve", []byte(`{"decision":"approve"}`)); err != nil {
		t.Fatalf("Signal approve: %v", err)
	}

	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		run, err := disk.Run(ctx, summary.ID)
		if err == nil && (run.Status == workflow.RunSucceeded || run.Status == workflow.RunFailed) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	run, err := disk.Run(ctx, summary.ID)
	if err != nil {
		t.Fatalf("read run: %v", err)
	}
	if run.Status != workflow.RunFailed {
		steps, _ := disk.Steps(ctx, summary.ID)
		for _, s := range steps {
			t.Logf("step %s: status=%s, err=%s", s.StepID, s.Status, s.ErrorCode)
		}
		t.Fatalf("run status = %s, want failed (the tool step must fail closed without arguments)", run.Status)
	}

	steps, err := disk.Steps(ctx, summary.ID)
	if err != nil {
		t.Fatalf("read steps: %v", err)
	}
	var toolStep *workflow.StepRun
	for i := range steps {
		if steps[i].StepID == "run_tool" {
			toolStep = steps[i]
		}
	}
	if toolStep == nil {
		t.Fatalf("run_tool step not found in %d steps", len(steps))
	}
	if toolStep.Status != workflow.StepFailed || toolStep.ErrorCode != string(workflow.ErrorCodeStepFailed) {
		t.Fatalf("run_tool step status = %s err = %s, want failed/workflow_step_failed", toolStep.Status, toolStep.ErrorCode)
	}
	if _, err := os.Stat(filepath.Join(workspace, ".workflow")); !os.IsNotExist(err) {
		t.Fatalf("placeholder payload file exists (err=%v): no canned payload may execute", err)
	}
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (n int, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestBuildWorkflowInterpreterEmitsWarningOnStatusPersistFailure(t *testing.T) {
	gf := writeWorkflowFixtures(t, validWorkflowYAML)
	ctx := t.Context()
	result, err := config.Load(gf.configPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	buf := &syncBuffer{}
	logger := slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	interpreter, closeStorage, err := buildWorkflowInterpreter(ctx, result.Config, logger)
	if err != nil {
		t.Fatalf("buildWorkflowInterpreter: %v", err)
	}
	defer closeStorage()

	db, err := openStorage(ctx, result.Config)
	if err != nil {
		t.Fatalf("open storage: %v", err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.ExecContext(ctx, `CREATE TRIGGER fail_status_update BEFORE UPDATE OF status ON workflow_run
WHEN NEW.status = 'suspended'
BEGIN
    SELECT RAISE(FAIL, 'forced status persist failure');
END;`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	summary, err := interpreter.Start(ctx, "demo", nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(buf.String(), "workflow run status persist failed") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	out := buf.String()
	if !strings.Contains(out, "workflow run status persist failed") {
		t.Fatalf("logged output = %q, want status persist failure warning", out)
	}
	if !strings.Contains(out, "forced status persist failure") {
		t.Fatalf("logged output = %q, want the forced trigger cause", out)
	}
	_ = summary
}
