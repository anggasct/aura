package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/platform/capability/capabilitytest"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/toolbroker"
	"github.com/anggasct/aura/internal/platform/turn/adk"
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

func serverCompositionOptions(result *config.LoadResult, logs io.Writer) *Options {
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
	if code, ok := CodeOf(app.Close(ctx)); !ok || code != ErrorCodeInvalidArgument {
		t.Fatalf("Close during Run = %v, want %s", code, ErrorCodeInvalidArgument)
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
		[]string{"memory_purge", "broadcast_resume", "schedule_resume", "channels", "retention_sweeps", "persona_eval_schedules"})
	requireNames(t, "lifecycle stopped", lifecycleNames(t, logs.String(), "lifecycle stopped"),
		[]string{"persona_eval_schedules", "retention_sweeps", "channels", "schedule_resume", "broadcast_resume", "memory_purge", "telemetry", "sqlite"})
}

func TestCompositionServerBuildFailureReleasesWhatWasBuilt(t *testing.T) {
	restoreDefaultLogger(t)
	result := loadCompositionConfig(t, writeCompositionConfig(t, freeLoopbackAddress(t), "", ""))
	logs := &syncBuffer{}
	app, err := Build(t.Context(), serverCompositionOptions(result, logs))
	if app != nil {
		t.Fatal("Build returned an app for a config without a primary model")
	}
	if code, ok := config.CodeOf(err); !ok || code != config.ErrorCodeConfigInvalid {
		t.Fatalf("Build error = %v, want %s", err, config.ErrorCodeConfigInvalid)
	}
	requireNames(t, "lifecycle stopped", lifecycleNames(t, logs.String(), "lifecycle stopped"), []string{"telemetry", "sqlite"})
}

func TestCompositionServerRegistersEveryConfiguredEntry(t *testing.T) {
	restoreDefaultLogger(t)
	t.Setenv("AURA_TEST_MODEL_KEY", "test-key")
	extra := fmt.Sprintf("profile:\n  enabled: true\ndurable:\n  enabled: true\n  mode: external\n  endpoint: http://%s\n  admin_endpoint: http://%s\n  handler_addr: %s\n",
		freeLoopbackAddress(t), freeLoopbackAddress(t), freeLoopbackAddress(t))
	result := loadCompositionConfig(t, writeCompositionConfig(t, freeLoopbackAddress(t), compositionModelsYAML, extra))
	app := buildApp(t, serverCompositionOptions(result, &syncBuffer{}))
	requireNames(t, "entries", app.group.Names(), []string{
		"sqlite", "telemetry", "memory_purge", "profile_extraction", "broadcast_resume", "schedule_resume",
		"channels", "workflow_proposal_cards", "retention_sweeps", "workflow_completion", "persona_eval_schedules",
	})
	if err := app.Close(t.Context()); err != nil {
		t.Fatalf("Close without Run: %v", err)
	}
}

func TestCompositionServerToolsHonourThePersonaCeiling(t *testing.T) {
	restoreDefaultLogger(t)
	t.Setenv("AURA_TEST_MODEL_KEY", "test-key")
	result := loadCompositionConfig(t, writeCompositionConfig(t, freeLoopbackAddress(t), compositionModelsYAML, compositionPersonaYAML))
	result.CapabilityReport = capabilitytest.Report(t, "workspace-read")
	app := buildApp(t, serverCompositionOptions(result, &syncBuffer{}))
	t.Cleanup(func() { _ = app.Close(context.Background()) })
	call := func(tool string) error {
		_, err := app.builtin.Execute(t.Context(), &runtimeadk.BuiltinToolRequest{
			RequestID: "call-" + tool, TurnID: "turn-1", SessionID: "sess-ceiling", PrincipalID: "friend",
			PrincipalClass: principal.ClassTrusted, AgentID: "desk", ToolName: tool, ToolVersion: "v1",
			Arguments: json.RawMessage(`{"path":"."}`), Capabilities: []string{"workspace-read"}, Trust: "derived_untrusted",
			Deadline: time.Now().Add(time.Minute), IdempotencyKey: "ceiling/" + tool, EventSequence: 1,
		})
		return err
	}
	if code, denied := toolbroker.CodeOf(call("read_file")); denied && code == toolbroker.ResultToolTierDenied {
		t.Fatal("read_file inside the persona ceiling was refused with tool_tier_denied")
	}
	if code, ok := toolbroker.CodeOf(call("list_dir")); !ok || code != toolbroker.ResultToolTierDenied {
		t.Fatalf("list_dir outside the persona ceiling = %v, want tool_tier_denied", code)
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
