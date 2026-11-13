//go:build linux

package app

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/modules/workflow"
	"github.com/anggasct/aura/internal/store"
	"github.com/anggasct/aura/internal/store/storetest"
)

func TestWorkflowStepOutsideTheCeilingIsRefused(t *testing.T) {
	dir := t.TempDir()
	workspace := filepath.Join(dir, "workspace")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}
	storetest.Seed(t, filepath.Join(dir, "aura.db"))
	db, err := store.OpenDB(t.Context(), filepath.Join(dir, "aura.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	boot := func(t *testing.T, personas ...config.AgentDefinition) (workflow.ToolRunner, *toolProbe) {
		t.Helper()
		cfg := config.Default()
		cfg.Tools.Workspace = workspace
		cfg.Storage.Path = dir
		cfg.Agents.Definitions = append(cfg.Agents.Definitions, personas...)
		registry, err := buildAgentRegistry(&cfg)
		if err != nil {
			t.Fatalf("buildAgentRegistry: %v", err)
		}
		runner := buildSurface(t, &surfaceInput{cfg: &cfg, db: db, registry: registry}).runner
		return runner, registerToolProbe(t, runner)
	}
	scribe := func(tools ...string) config.AgentDefinition {
		return config.AgentDefinition{ID: "scribe", Description: "d", Instructions: "i", Tools: tools}
	}
	callAs := func(runID string) *workflow.StepCall {
		call := ownerStepCall(runID, "step-1", 0)
		call.Origin.AgentID = "scribe"
		return call
	}

	runner, probe := boot(t, scribe())
	if _, err := runner.Invoke(stepContextFor(t, db, callAs("run-ceiling")), probeTool, probeArgs("before")); err != nil {
		t.Fatalf("Invoke before the restart: %v", err)
	}
	if got := len(probe.seen()); got != 1 {
		t.Fatalf("the tool ran %d times before the restart, want 1", got)
	}

	t.Run("a restart that shrinks the ceiling refuses the next step", func(t *testing.T) {
		runner, probe := boot(t, scribe("read_file"))
		call := callAs("run-ceiling")
		call.StepID = "step-2"
		_, err := runner.Invoke(stepContextFor(t, db, call), probeTool, probeArgs("after"))
		if code, ok := workflow.CodeOf(err); !ok || code != workflow.ErrorCodeCapabilityExceeded {
			t.Fatalf("Invoke = %v, want %s", err, workflow.ErrorCodeCapabilityExceeded)
		}
		if got := len(probe.seen()); got != 0 {
			t.Fatalf("the broker ran the tool %d times outside the ceiling, want 0", got)
		}
	})

	t.Run("a restart that removes the persona refuses the next step", func(t *testing.T) {
		runner, probe := boot(t)
		call := callAs("run-ceiling")
		call.StepID = "step-3"
		_, err := runner.Invoke(stepContextFor(t, db, call), probeTool, probeArgs("gone"))
		if code, ok := workflow.CodeOf(err); !ok || code != workflow.ErrorCodePersonaMissing {
			t.Fatalf("Invoke = %v, want %s", err, workflow.ErrorCodePersonaMissing)
		}
		if got := len(probe.seen()); got != 0 {
			t.Fatalf("the broker ran the tool %d times for a missing persona, want 0", got)
		}
	})
}
