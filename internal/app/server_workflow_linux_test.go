//go:build linux

package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/anggasct/aura/internal/modules/workflow"
	"github.com/anggasct/aura/internal/platform/capability/capabilitytest"
)

const twoReadStepsYAML = `id: two-reads
version: 1
goal: Read two files
source: defined
steps:
  - id: first
    executor: { kind: tool, tool: read_file, args: { path: first.txt } }
    timeout: 1m
  - id: second
    depends_on: [first]
    executor: { kind: tool, tool: read_file, args: { path: second.txt } }
    timeout: 1m
`

func writeWorkflowServerConfig(t *testing.T) string {
	t.Helper()
	definitions := t.TempDir()
	if err := os.WriteFile(filepath.Join(definitions, "two-reads.yaml"), []byte(twoReadStepsYAML), 0o600); err != nil {
		t.Fatalf("write definition: %v", err)
	}
	extra := fmt.Sprintf("workflows:\n  definitions_dir: %s\n", definitions)
	path := writeCompositionConfig(t, freeLoopbackAddress(t), compositionModelsYAML, extra)
	workspace := loadCompositionConfig(t, path).Config.Tools.Workspace
	for _, name := range []string{"first.txt", "second.txt"} {
		if err := os.WriteFile(filepath.Join(workspace, name), []byte(name), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return path
}

func TestServerHostsWorkflowRunsTheCLIStarts(t *testing.T) {
	restoreDefaultLogger(t)
	t.Setenv("AURA_TEST_MODEL_KEY", "test-key")
	path := writeWorkflowServerConfig(t)
	serverResult := loadCompositionConfig(t, path)
	serverResult.CapabilityReport = capabilitytest.Report(t, "workspace-read")
	srv := &compositionServer{app: buildApp(t, serverCompositionOptions(serverResult, &syncBuffer{}))}
	t.Cleanup(func() {
		if srv.ran != nil {
			srv.cancel()
			<-srv.ran
		}
		_ = srv.app.Close(context.Background())
	})
	srv.run(t)

	command := buildApp(t, commandCompositionOptions(loadCompositionConfig(t, path), &syncBuffer{}, PartWorkflows))
	t.Cleanup(func() { _ = command.Close(context.Background()) })
	summary, err := command.interpreter.Start(t.Context(), "two-reads", nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	db := compositionDatabase(t, path)
	if got, want := awaitRunEnded(t, db, summary.DurableKey), (storedRun{handler: workflow.HandlerName, state: "succeeded"}); got != want {
		t.Fatalf("workflow run = %+v, want %+v", got, want)
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM workflow_run WHERE id = ? AND status = ?`, summary.ID, workflow.RunSucceeded); n != 1 {
		t.Fatalf("succeeded workflow runs = %d, want 1", n)
	}
	for _, step := range []string{"first", "second"} {
		action := "run/" + summary.ID + "/step/" + step + "/attempt/0"
		if n := countRows(t, db, `SELECT COUNT(*) FROM durable_action WHERE run_key = ? AND action_key = ?`, summary.DurableKey, action); n != 1 {
			t.Fatalf("journal actions %q = %d, want 1", action, n)
		}
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM workflow_step_run WHERE run_id = ? AND status = ?`, summary.ID, workflow.StepSucceeded); n != 2 {
		t.Fatalf("succeeded steps = %d, want 2", n)
	}
	srv.stopRun(t)
}
