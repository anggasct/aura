package app

import (
	"bytes"
	"context"
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
	toolsbuiltin "github.com/anggasct/aura/internal/modules/builtin"
	"github.com/anggasct/aura/internal/modules/workflow"
	auraagent "github.com/anggasct/aura/internal/platform/agent"
	"github.com/anggasct/aura/internal/platform/durable"
	"github.com/anggasct/aura/internal/platform/durable/durabletest"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/store"
)

func writeWorkflowFixtures(t *testing.T, definition string) *globalFlags {
	t.Helper()
	return writeWorkflowFixturesWith(t, definition, "")
}

func writeWorkflowFixturesWith(t *testing.T, definition, extraConfig string) *globalFlags {
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
` + extraConfig
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, []byte(cfg), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	seedConfiguredStorage(t, configPath)
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
	restoreDefaultLogger(t)
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
	interpreter := workflow.NewInterpreter(disk, queuedRuntime{durabletest.NewFake()}, nil)
	if err := interpreter.Install(ctx, []*workflow.Spec{spec}, &workflow.ValidationDeps{KnownTools: toolsbuiltin.DefinitionNames(), Agents: builtinRegistry(t), CheckToolCall: acceptToolCall, StepAllowed: acceptStepAllowed}); err != nil {
		t.Fatalf("install definition: %v", err)
	}
	summary, err := interpreter.StartConfigured(ctx, spec.ID, nil, principal.OwnerID, principal.ClassOwner, "")
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
	interpreter := workflow.NewInterpreter(disk, queuedRuntime{durabletest.NewFake()}, nil)
	if err := interpreter.Install(ctx, []*workflow.Spec{spec}, &workflow.ValidationDeps{KnownTools: toolsbuiltin.DefinitionNames(), Agents: builtinRegistry(t), CheckToolCall: acceptToolCall, StepAllowed: acceptStepAllowed}); err != nil {
		t.Fatalf("install definition: %v", err)
	}
	summary, err := interpreter.StartConfigured(ctx, spec.ID, nil, principal.OwnerID, principal.ClassOwner, "")
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

const (
	stepDefinitionID      = "definition-1"
	stepDefinitionDigest  = "digest-1"
	stepDefinitionVersion = 1
)

func ownerStepCall(runID, stepID string, attempt int) *workflow.StepCall {
	return &workflow.StepCall{
		RunID: runID, StepID: stepID, Attempt: attempt,
		Origin: workflow.RunOrigin{
			PrincipalID: principal.OwnerID, PrincipalClass: string(principal.ClassOwner), AgentID: auraagent.DefaultID,
		},
		DefinitionDigest: stepDefinitionDigest, DefinitionID: stepDefinitionID, DefinitionVersion: stepDefinitionVersion,
		Approval: workflow.Approval{
			Kind: workflow.ApprovalInstalled, Digest: stepDefinitionDigest, DefinitionID: stepDefinitionID, DefinitionVersion: stepDefinitionVersion,
		},
	}
}

func seedRunSession(t *testing.T, db *sql.DB, call *workflow.StepCall) {
	t.Helper()
	tx, err := db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	sess := &store.Session{ID: workflow.RunSessionID(call.RunID), OwnerID: call.Origin.PrincipalID, AgentID: call.Origin.AgentID}
	if err := store.CreateSessionTx(t.Context(), tx, sess); err != nil {
		if code, ok := store.CodeOf(err); ok && code == store.ErrorCodeSessionIDConflict {
			return
		}
		t.Fatalf("create run session: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

func stepContextFor(t *testing.T, db *sql.DB, call *workflow.StepCall) context.Context {
	t.Helper()
	seedRunSession(t, db, call)
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	t.Cleanup(cancel)
	return workflow.WithStepCall(ctx, call)
}

func stepCallContext(t *testing.T, db *sql.DB) context.Context {
	t.Helper()
	return stepContextFor(t, db, ownerStepCall("run-1", "step-1", 0))
}

func newWorkflowToolRunnerFixture(t *testing.T) (workflow.ToolRunner, *sql.DB, string) {
	t.Helper()
	return newWorkflowToolRunnerFixtureWith(t, nil)
}

func newWorkflowToolRunnerFixtureWith(t *testing.T, configure func(*config.Config)) (workflow.ToolRunner, *sql.DB, string) {
	t.Helper()
	rig := newSurfaceRig(t, &surfaceRigOptions{configure: configure})
	return rig.runner, rig.db, rig.workspace
}

func TestWorkflowToolRunnerExecutesCoveredEffectfulTool(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("builtin tool executor requires Linux")
	}
	runner, db, workspace := newWorkflowToolRunnerFixture(t)
	args := json.RawMessage(`{"path":"notes.txt","content":"authored by the workflow step"}`)
	output, err := runner.Invoke(stepCallContext(t, db), "write_file", args)
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

func TestWorkflowToolRunnerRejectsEmptyArgumentsFailClosed(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("builtin tool executor requires Linux")
	}
	runner, db, workspace := newWorkflowToolRunnerFixture(t)
	for _, args := range []json.RawMessage{nil, json.RawMessage(`{}`), json.RawMessage(`null`)} {
		_, err := runner.Invoke(stepCallContext(t, db), "write_file", args)
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

func TestWorkflowToolRunnerRunsNoPlaceholderPayloadOverInterpreter(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("builtin tool executor requires Linux")
	}
	runner, db, workspace := newWorkflowToolRunnerFixture(t)

	toolPtr := func(s string) *string { return &s }
	fake := durabletest.NewFake()
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
	deps := &workflow.ValidationDeps{
		Agents:         builtinRegistry(t),
		KnownTools:     toolsbuiltin.DefinitionNames(),
		EffectfulTools: toolsbuiltin.EffectfulToolNames(),
		CheckToolCall:  acceptToolCall,
		StepAllowed:    acceptStepAllowed,
	}
	ctx := t.Context()
	if err := interpreter.Install(ctx, []*workflow.Spec{spec}, deps); err != nil {
		t.Fatalf("Load: %v", err)
	}

	summary, err := interpreter.StartConfigured(ctx, spec.ID, nil, principal.OwnerID, principal.ClassOwner, "")
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

	restoreDefaultLogger(t)
	buf := &syncBuffer{}
	app, err := Build(ctx, &Options{
		Surface: SurfaceCommand, Config: &result, LogLevel: "warn", LogFormat: "text", LogOutput: buf,
		Command: CommandOptions{Parts: []Part{PartWorkflows}},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer func() { _ = app.Close(ctx) }()
	interpreter := app.interpreter
	runHost, ok := app.runtime.(durableRunHost)
	if !ok {
		t.Fatal("the command build's durable runtime cannot host runs")
	}
	stage := &durableHostStage{host: runHost, handlers: []string{workflow.HandlerName}, logger: app.logger}
	if err := stage.start(ctx); err != nil {
		t.Fatalf("start the durable host: %v", err)
	}
	defer func() { _ = stage.stop(ctx) }()

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

	summary, err := interpreter.StartConfigured(ctx, "demo", nil, principal.OwnerID, principal.ClassOwner, "")
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

func TestWorkflowApproveRequiresStep(t *testing.T) {
	gf := writeWorkflowFixtures(t, validWorkflowYAML)
	if _, err := runWorkflowCommand(t, gf, "approve", "run-1"); err == nil {
		t.Fatal("expected missing --step to fail, got nil")
	}
}

func TestWorkflowSignalValidatesPayload(t *testing.T) {
	gf := writeWorkflowFixtures(t, validWorkflowYAML)
	if _, err := runWorkflowCommand(t, gf, "signal", "run-1", "--step", "hold", "--payload", "{oops"); err == nil {
		t.Fatal("expected invalid payload to fail, got nil")
	}
}

func TestWorkflowApproveUnknownRunFailsClosed(t *testing.T) {
	gf := writeWorkflowFixtures(t, validWorkflowYAML)
	if _, err := runWorkflowCommand(t, gf, "approve", "run-missing", "--step", "gate"); err == nil {
		t.Fatal("expected unknown run approval to fail, got nil")
	}
}

func TestWorkflowSignalUnknownRunFailsClosed(t *testing.T) {
	gf := writeWorkflowFixtures(t, validWorkflowYAML)
	if _, err := runWorkflowCommand(t, gf, "signal", "run-missing", "--step", "hold", "--payload", `{}`); err == nil {
		t.Fatal("expected unknown run signal to fail, got nil")
	}
}

func TestWorkflowInspectListsCorrelations(t *testing.T) {
	gf := writeWorkflowFixtures(t, validWorkflowYAML)
	ctx := t.Context()
	result, err := config.Load(gf.configPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	db, err := openStorage(ctx, result.Config)
	if err != nil {
		t.Fatalf("open storage: %v", err)
	}
	disk := workflow.NewStore(db)
	dir := filepath.Dir(gf.configPath)
	spec, err := workflow.LoadSpecFile(filepath.Join(dir, "workflows", "demo.yaml"))
	if err != nil {
		t.Fatalf("load fixture spec: %v", err)
	}
	interpreter := workflow.NewInterpreter(disk, queuedRuntime{durabletest.NewFake()}, nil)
	if err := interpreter.Install(ctx, []*workflow.Spec{spec}, &workflow.ValidationDeps{KnownTools: toolsbuiltin.DefinitionNames(), Agents: builtinRegistry(t), CheckToolCall: acceptToolCall, StepAllowed: acceptStepAllowed}); err != nil {
		t.Fatalf("install definition: %v", err)
	}
	summary, err := interpreter.StartConfigured(ctx, spec.ID, nil, principal.OwnerID, principal.ClassOwner, "")
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	if err := disk.BindCorrelation(ctx, &workflow.Correlation{
		Source: "github", EventType: "check_suite.completed", ExternalID: "org/repo#42",
		RunID: summary.ID, SignalName: "wait.hold", DedupeKey: "",
	}); err != nil {
		t.Fatalf("bind correlation: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}
	out, err := runWorkflowCommand(t, gf, "inspect", summary.ID)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	for _, want := range []string{"correlation:", "org/repo#42", "awaiting"} {
		if !strings.Contains(out, want) {
			t.Errorf("inspect output = %q, want %q", out, want)
		}
	}
}
