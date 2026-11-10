package app

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/modules/codeexec/interpreter"
	"github.com/anggasct/aura/internal/platform/health"
	"github.com/anggasct/aura/internal/store"
	"github.com/anggasct/aura/internal/store/storetest"
)

func TestEnableExecutionBuildsTheRunnerAndVerifier(t *testing.T) {
	defaults := config.DefaultInterpreter()
	rig := newAvailableInterpreterRig(t)
	runtime, err := newInterpreterRuntime(&config.Config{Storage: config.Storage{Path: rig.dataRoot}, Interpreter: defaults}, rig.manifest)
	if err != nil {
		t.Fatalf("newInterpreterRuntime: %v", err)
	}
	if runtime.runner != nil {
		t.Fatal("execution must stay off until it is enabled")
	}
	if err := runtime.enableExecution(t.Context(), &config.Config{Storage: config.Storage{Path: rig.dataRoot}, Interpreter: defaults}, testArtifacts(t), nil, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatalf("enableExecution: %v", err)
	}
	if runtime.runner == nil || runtime.verifier == nil {
		t.Fatal("enableExecution left the runner or the verifier unset")
	}
}

func TestEnableExecutionSweepsStaleRunDirectories(t *testing.T) {
	rig := newAvailableInterpreterRig(t)
	cfg := &config.Config{Storage: config.Storage{Path: rig.dataRoot}, Interpreter: config.DefaultInterpreter()}
	dataRoot, err := resolveDataRoot(cfg)
	if err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(dataRoot, interpreterRunsDirName, "run-stale", "output")
	if err := os.MkdirAll(stale, 0o700); err != nil {
		t.Fatal(err)
	}
	runtime, err := newInterpreterRuntime(cfg, rig.manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.enableExecution(t.Context(), cfg, testArtifacts(t), nil, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatalf("enableExecution: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(stale)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the stale run directory survived startup: %v", err)
	}
}

func TestEnableExecutionRejectsAnInvalidLimit(t *testing.T) {
	rig := newAvailableInterpreterRig(t)
	invalid := config.DefaultInterpreter()
	invalid.MaxProcesses = 0
	cfg := &config.Config{Storage: config.Storage{Path: rig.dataRoot}, Interpreter: invalid}
	runtime, err := newInterpreterRuntime(cfg, rig.manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.enableExecution(t.Context(), cfg, testArtifacts(t), nil, slog.New(slog.DiscardHandler)); err == nil {
		t.Fatal("enableExecution accepted a zero process limit")
	}
	if runtime.runner != nil {
		t.Fatal("a rejected configuration left a runner behind")
	}
}

func TestHealthReasonForAFailedProbeMatchesTheVerifierReason(t *testing.T) {
	if health.InterpreterReasonProbeFailed != string(interpreter.ReasonProbeFailed) {
		t.Fatalf("health reason %q differs from the verifier reason %q", health.InterpreterReasonProbeFailed, interpreter.ReasonProbeFailed)
	}
	rows := healthRuntimes([]interpreter.Verification{{Runtime: "python", Version: "3", State: interpreter.StateInvalid, Reason: interpreter.ReasonProbeFailed}})
	if len(rows) != 1 || rows[0].State != health.InterpreterRuntimeInvalid || rows[0].Reason != health.InterpreterReasonProbeFailed {
		t.Fatalf("rows = %+v", rows)
	}
}

func testArtifacts(t *testing.T) store.ArtifactStore {
	t.Helper()
	return store.NewArtifactStore(storetest.OpenMigrated(t), t.TempDir(), store.DefaultArtifactQuotaBytes)
}
