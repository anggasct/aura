package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	appconfig "github.com/anggasct/aura/internal/app/config"
	"github.com/anggasct/aura/internal/modules/knowledge"
	"github.com/anggasct/aura/internal/modules/workflow"
	"github.com/anggasct/aura/internal/platform/capability/capabilitytest"
	"github.com/anggasct/aura/internal/platform/configkit"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/toolbroker"
	runtimeadk "github.com/anggasct/aura/internal/platform/turn/adk"
)

const compositionPersonaYAML = `agents:
  definitions:
    - id: desk
      description: trusted desk
      instructions: "You answer trusted callers."
      audience: trusted
      tools: [read_file]
      principal_tools: [read_file]
access:
  budgets:
    trusted:
      max_tokens_per_day: 100000
      max_turns_per_minute: 50
`

func writeCompositionConfig(t *testing.T, probeAddr, models, extra string) string {
	t.Helper()
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}
	content := fmt.Sprintf("version: 1\nstorage:\n  path: %s\ntools:\n  workspace: %s\nhealth:\n  listen: %s\n", filepath.Join(root, "data"), workspace, probeAddr) + models + extra
	path := filepath.Join(root, "aura.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func serverCompositionOptions(result *appconfig.LoadResult, logs io.Writer) *Options {
	return &Options{Surface: SurfaceServer, Config: result, LogLevel: "debug", LogFormat: "json", LogOutput: logs}
}

func TestCompositionServerStartsAndStopsInOrder(t *testing.T) {
	restoreDefaultLogger(t)
	t.Setenv("AURA_TEST_MODEL_KEY", "test-key")
	probeAddr := freeLoopbackAddress(t)
	result := loadCompositionConfig(t, writeCompositionConfig(t, probeAddr, compositionModelsYAML, ""))
	logs := &syncBuffer{}
	app := buildApp(t, serverCompositionOptions(result, logs))

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	ran := make(chan error, 1)
	go func() { ran <- app.Run(ctx) }()
	waitForProbe(t, probeAddr, "/livez")
	response, err := probeGet(ctx, probeAddr, "/livez")
	if err != nil {
		t.Fatalf("GET /livez: %v", err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("/livez status = %d, want 200", response.StatusCode)
	}
	if err := app.Close(ctx); !fault.HasCode(err, ErrorCodeInvalidArgument) {
		t.Fatalf("Close during Run = %v, want %s", err, ErrorCodeInvalidArgument)
	}
	cancel()
	select {
	case err := <-ran:
		if err != nil {
			t.Fatalf("Run after cancel = %v, want nil", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not return after the context was cancelled")
	}
	if err := app.Close(t.Context()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	requireNames(t, "lifecycle started", lifecycleNames(t, logs.String(), "lifecycle started"),
		[]string{"legacy_taint", "memory_purge", "broadcast_resume", "schedule_resume", "schedule_pass", "turn_recovery", "channels", "durable_host", "turn_engine", "turn_recovery_pass", "workflow_proposal_cards", "retention_sweeps", "run_retention", "workflow_completion", "persona_eval_schedules"})
	requireNames(t, "lifecycle stopped", lifecycleNames(t, logs.String(), "lifecycle stopped"),
		[]string{"persona_eval_schedules", "workflow_completion", "run_retention", "retention_sweeps", "workflow_proposal_cards", "turn_recovery_pass", "turn_engine", "durable_host", "channels", "turn_recovery", "schedule_pass", "schedule_resume", "broadcast_resume", "memory_purge", "legacy_taint", "telemetry", "sqlite"})
}

func TestCompositionServerBuildFailureReleasesWhatWasBuilt(t *testing.T) {
	restoreDefaultLogger(t)
	result := loadCompositionConfig(t, writeCompositionConfig(t, freeLoopbackAddress(t), "", ""))
	logs := &syncBuffer{}
	app, err := Build(t.Context(), serverCompositionOptions(result, logs))
	if app != nil {
		t.Fatal("Build returned an app for a config without a primary model")
	}
	if !fault.HasCode(err, configkit.ErrorCodeInvalid) {
		t.Fatalf("Build error = %v, want %s", err, configkit.ErrorCodeInvalid)
	}
	requireNames(t, "lifecycle stopped", lifecycleNames(t, logs.String(), "lifecycle stopped"), []string{"telemetry", "sqlite"})
}

func TestCompositionServerRegistersEveryConfiguredEntry(t *testing.T) {
	restoreDefaultLogger(t)
	t.Setenv("AURA_TEST_MODEL_KEY", "test-key")
	result := loadCompositionConfig(t, writeCompositionConfig(t, freeLoopbackAddress(t), compositionModelsYAML, "profile:\n  enabled: true\n"))
	app := buildApp(t, serverCompositionOptions(result, &syncBuffer{}))
	requireNames(t, "entries", app.group.Names(), []string{
		"sqlite", "telemetry", "legacy_taint", "memory_purge", "profile_extraction", "broadcast_resume", "schedule_resume",
		"schedule_pass", "turn_recovery", "channels", "durable_host", "turn_engine", "turn_recovery_pass", "workflow_proposal_cards", "retention_sweeps", "run_retention", "workflow_completion", "persona_eval_schedules",
	})
	if err := app.Close(t.Context()); err != nil {
		t.Fatalf("Close without Run: %v", err)
	}
}

func requirePersonaCeiling(t *testing.T, app *App) {
	t.Helper()
	call := func(tool string) error {
		_, err := app.builtin.Execute(t.Context(), &runtimeadk.BuiltinToolRequest{
			RequestID: "call-" + tool, TurnID: "turn-1", SessionID: "sess-ceiling", PrincipalID: "friend",
			PrincipalClass: principal.ClassTrusted, AgentID: "desk", ToolName: tool, ToolVersion: "v1",
			Arguments: json.RawMessage(`{"path":"."}`), Capabilities: []string{"workspace-read"}, Trust: "derived_untrusted",
			Deadline: time.Now().Add(time.Minute), IdempotencyKey: "ceiling/" + tool,
		})
		return err
	}
	if fault.HasCode(call("read_file"), toolbroker.ResultToolTierDenied) {
		t.Fatal("read_file inside the persona ceiling was refused with tool_tier_denied")
	}
	if err := call("list_dir"); !fault.HasCode(err, toolbroker.ResultToolTierDenied) {
		t.Fatalf("list_dir outside the persona ceiling = %v, want tool_tier_denied", err)
	}
}

func TestCompositionServerToolsHonourThePersonaCeiling(t *testing.T) {
	restoreDefaultLogger(t)
	t.Setenv("AURA_TEST_MODEL_KEY", "test-key")
	result := loadCompositionConfig(t, writeCompositionConfig(t, freeLoopbackAddress(t), compositionModelsYAML, compositionPersonaYAML))
	result.CapabilityReport = capabilitytest.Report(t, "workspace-read")
	app := buildApp(t, serverCompositionOptions(result, &syncBuffer{}))
	t.Cleanup(func() { _ = app.Close(context.Background()) })
	requirePersonaCeiling(t, app)
}

func TestCompositionServerWithWorkflowsKeepsThePersonaCeiling(t *testing.T) {
	restoreDefaultLogger(t)
	t.Setenv("AURA_TEST_MODEL_KEY", "test-key")
	definitions := t.TempDir()
	if err := os.WriteFile(filepath.Join(definitions, "demo.yaml"), []byte(validWorkflowYAML), 0o600); err != nil {
		t.Fatalf("write definition: %v", err)
	}
	extra := compositionPersonaYAML + fmt.Sprintf("workflows:\n  definitions_dir: %s\n", definitions)
	result := loadCompositionConfig(t, writeCompositionConfig(t, freeLoopbackAddress(t), compositionModelsYAML, extra))
	result.CapabilityReport = capabilitytest.Report(t, "workspace-read")
	app := buildApp(t, serverCompositionOptions(result, &syncBuffer{}))
	t.Cleanup(func() { _ = app.Close(context.Background()) })
	requirePersonaCeiling(t, app)
}

func TestCompositionServerRefusesABrokenWorkflowDefinition(t *testing.T) {
	restoreDefaultLogger(t)
	t.Setenv("AURA_TEST_MODEL_KEY", "test-key")
	definitions := t.TempDir()
	broken := "id: broken\nversion: 1\ngoal: g\nsteps:\n  - id: a\n    executor: { kind: shell }\n    timeout: 1m\n"
	if err := os.WriteFile(filepath.Join(definitions, "broken.yaml"), []byte(broken), 0o600); err != nil {
		t.Fatalf("write definition: %v", err)
	}
	extra := fmt.Sprintf("workflows:\n  definitions_dir: %s\n", definitions)
	result := loadCompositionConfig(t, writeCompositionConfig(t, freeLoopbackAddress(t), compositionModelsYAML, extra))
	_, err := Build(t.Context(), serverCompositionOptions(result, &syncBuffer{}))
	if code, ok := fault.CodeOf[workflow.ErrorCode](err); !ok || code != workflow.ErrorCodeExecutorInvalid {
		t.Fatalf("Build error = %v, want %s", err, workflow.ErrorCodeExecutorInvalid)
	}
}

func TestCompositionCloseWithoutRunReleasesResources(t *testing.T) {
	restoreDefaultLogger(t)
	t.Setenv("AURA_TEST_MODEL_KEY", "test-key")
	result := loadCompositionConfig(t, writeCompositionConfig(t, freeLoopbackAddress(t), compositionModelsYAML, ""))
	logs := &syncBuffer{}
	app := buildApp(t, serverCompositionOptions(result, logs))
	if err := app.Close(t.Context()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := app.Close(t.Context()); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	requireNames(t, "lifecycle stopped", lifecycleNames(t, logs.String(), "lifecycle stopped"), []string{"telemetry", "sqlite"})
	if started := lifecycleNames(t, logs.String(), "lifecycle started"); len(started) != 0 {
		t.Fatalf("lifecycle started = %v, want none", started)
	}
}

const compositionKnowledgePersonaYAML = `agents:
  definitions:
    - id: lobby
      description: front desk
      instructions: "Help visitors."
      audience: public
      knowledge: [faq]
`

func TestCompositionServerWithKnowledgeDiscoversModuleContracts(t *testing.T) {
	restoreDefaultLogger(t)
	t.Setenv("AURA_TEST_MODEL_KEY", "test-key")
	probeAddr := freeLoopbackAddress(t)
	result := loadCompositionConfig(t, writeCompositionConfig(t, probeAddr, compositionModelsYAML, compositionKnowledgePersonaYAML))
	logs := &syncBuffer{}

	b := newBuild(serverCompositionOptions(result, logs))
	for _, step := range b.phases(t.Context()) {
		if err := step(); err != nil {
			if b.group != nil {
				_ = b.group.Stop(t.Context())
			}
			t.Fatalf("build phase: %v", err)
		}
	}
	app := &App{
		surface: SurfaceServer, group: b.group, builtin: b.builtin, serve: b.serve,
		logger: b.logger, db: b.db, turns: b.turns, result: b.result,
		runtime: b.runtime, interpreter: b.interpreter, models: b.models, knowledge: b.knowledge,
	}

	probeBuild := &build{cfg: b.cfg, group: b.group}
	probeMod, err := knowledge.New(t.Context(), &knowledge.ModuleDeps{
		Config: b.cfg.Knowledge, DBPath: b.cfg.Storage.Path + "/aura.db",
	})
	if err != nil {
		t.Fatalf("knowledge.New probe: %v", err)
	}
	if err := probeBuild.registerModule("knowledge-probe", probeMod); err != nil {
		t.Fatalf("registerModule probe: %v", err)
	}
	if len(probeBuild.execOpts) != 1 {
		t.Fatalf("registerModule execOpts = %d, want 1 (ContextProvider)", len(probeBuild.execOpts))
	}

	output, err := app.builtin.Execute(t.Context(), &runtimeadk.BuiltinToolRequest{
		RequestID: "call-kb", TurnID: "turn-1", SessionID: "sess-kb", PrincipalID: principal.OwnerID,
		PrincipalClass: principal.ClassOwner, AgentID: "lobby", ToolName: "kb_search", ToolVersion: "v1",
		Arguments: json.RawMessage(`{"query":"refunds"}`), Trust: "derived_untrusted",
		Deadline: time.Now().Add(time.Minute), IdempotencyKey: "kb/owner",
		Knowledge: []string{"faq"},
	})
	if err != nil {
		t.Fatalf("kb_search via discovered ToolProvider failed: %v", err)
	}
	var searchOut knowledge.SearchOutput
	if err := json.Unmarshal(output.Output, &searchOut); err != nil {
		t.Fatalf("decode kb_search output %s: %v", output.Output, err)
	}

	checkIDs := map[string]bool{}
	for i := range b.server.checks {
		checkIDs[b.server.checks[i].ID] = true
	}
	if !checkIDs[knowledge.ComponentKnowledge] || !checkIDs[knowledge.ComponentKnowledgeEmbedding] {
		t.Fatalf("discovered server health checks = %v, want %q and %q", checkIDs, knowledge.ComponentKnowledge, knowledge.ComponentKnowledgeEmbedding)
	}

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	ran := make(chan error, 1)
	go func() { ran <- app.Run(ctx) }()
	waitForProbe(t, probeAddr, "/livez")

	response, err := probeGet(ctx, probeAddr, "/readyz")
	if err != nil {
		t.Fatalf("GET /readyz: %v", err)
	}
	_ = response.Body.Close()

	cancel()
	select {
	case err := <-ran:
		if err != nil {
			t.Fatalf("Run after cancel = %v, want nil", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
	if err := app.Close(t.Context()); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestCompositionServerWithKnowledgeVectorsStopsLoaderBeforePools(t *testing.T) {
	restoreDefaultLogger(t)
	t.Setenv("AURA_TEST_MODEL_KEY", "test-key")
	probeAddr := freeLoopbackAddress(t)
	extra := compositionKnowledgePersonaYAML + `knowledge:
  embedding:
    base_url: https://api.example.com/v1
    model: embed-v1
    dimensions: 4
`
	result := loadCompositionConfig(t, writeCompositionConfig(t, probeAddr, compositionModelsYAML, extra))
	logs := &syncBuffer{}
	app := buildApp(t, serverCompositionOptions(result, logs))

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	ran := make(chan error, 1)
	go func() { ran <- app.Run(ctx) }()
	waitForProbe(t, probeAddr, "/livez")
	cancel()
	select {
	case err := <-ran:
		if err != nil {
			t.Fatalf("Run after cancel = %v, want nil", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
	if err := app.Close(t.Context()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	stopped := lifecycleNames(t, logs.String(), "lifecycle stopped")
	vectorsIdx := slices.Index(stopped, "knowledge_vectors")
	poolsIdx := slices.Index(stopped, "knowledge")
	if vectorsIdx < 0 || poolsIdx < 0 || vectorsIdx >= poolsIdx {
		t.Fatalf("stopped order = %v, want knowledge_vectors before knowledge", stopped)
	}
}

func TestCompositionCloseWithoutRunClosesKnowledgePools(t *testing.T) {
	restoreDefaultLogger(t)
	t.Setenv("AURA_TEST_MODEL_KEY", "test-key")
	result := loadCompositionConfig(t, writeCompositionConfig(t, freeLoopbackAddress(t), compositionModelsYAML, compositionKnowledgePersonaYAML))
	logs := &syncBuffer{}
	app := buildApp(t, serverCompositionOptions(result, logs))
	if err := app.Close(t.Context()); err != nil {
		t.Fatalf("Close without Run: %v", err)
	}
	stopped := lifecycleNames(t, logs.String(), "lifecycle stopped")
	if !slices.Contains(stopped, "knowledge") {
		t.Fatalf("stopped = %v, want knowledge defer to close pools even without Run", stopped)
	}
}

func TestMCPCloseWithoutRunClosesSubprocesses(t *testing.T) {
	restoreDefaultLogger(t)
	t.Setenv("AURA_TEST_MODEL_KEY", "test-key")
	extra := "mcp:\n  servers:\n    - name: docs\n      transport: stdio\n      command: /usr/local/bin/docs-mcp\n"
	result := loadCompositionConfig(t, writeCompositionConfig(t, freeLoopbackAddress(t), compositionModelsYAML, extra))
	logs := &syncBuffer{}
	app := buildApp(t, serverCompositionOptions(result, logs))
	if err := app.Close(t.Context()); err != nil {
		t.Fatalf("Close without Run: %v", err)
	}
	stopped := lifecycleNames(t, logs.String(), "lifecycle stopped")
	if !slices.Contains(stopped, "mcp") {
		t.Fatalf("stopped = %v, want mcp defer to close subprocesses even without Run", stopped)
	}
}

func TestCompositionChatWithToolsAndNoEnabledCapabilities(t *testing.T) {
	restoreDefaultLogger(t)
	t.Setenv("AURA_TEST_MODEL_KEY", "test-key")
	extra := fmt.Sprintf("tools:\n  workspace: %s\n", t.TempDir())
	result := loadCompositionConfig(t, writeChatCompositionConfig(t, extra))
	app := buildApp(t, chatCompositionOptions(result, &syncBuffer{}))
	if err := app.Run(t.Context()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := app.Close(t.Context()); err != nil {
		t.Fatalf("Close: %v", err)
	}
}
