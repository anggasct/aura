package app

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/anggasct/aura/internal/modules/codeexec"
	"github.com/anggasct/aura/internal/modules/codeexec/interpreter"
	"github.com/anggasct/aura/internal/platform/sqlite/sqlitetest"
	"github.com/anggasct/aura/internal/platform/storage"
)

func TestEnableExecutionBuildsTheRunnerAndVerifier(t *testing.T) {
	defaults := codeexec.Default()
	rig := newAvailableInterpreterRig(t)
	mod, err := codeexec.New(t.Context(), &codeexec.ModuleDeps{
		Config:    &defaults,
		DataRoot:  rig.dataRoot,
		Manifest:  rig.manifest,
		Artifacts: testArtifacts(t),
		Logger:    slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("codeexec.New: %v", err)
	}
	if mod.Runner() == nil || mod.Store() == nil || mod.Verified() == nil {
		t.Fatal("codeexec.New left the runner, store, or verified set unset")
	}
	if len(mod.Tools()) != 1 || len(mod.HealthChecks()) != 1 {
		t.Fatalf("tools=%d health=%d, want 1 each", len(mod.Tools()), len(mod.HealthChecks()))
	}
}

func TestEnableExecutionSweepsStaleRunDirectories(t *testing.T) {
	rig := newAvailableInterpreterRig(t)
	defaults := codeexec.Default()
	stale := filepath.Join(rig.dataRoot, codeexec.RunsDirName, "run-stale", "output")
	if err := os.MkdirAll(stale, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := codeexec.New(t.Context(), &codeexec.ModuleDeps{
		Config:    &defaults,
		DataRoot:  rig.dataRoot,
		Manifest:  rig.manifest,
		Artifacts: testArtifacts(t),
		Logger:    slog.New(slog.DiscardHandler),
	}); err != nil {
		t.Fatalf("codeexec.New: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(stale)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the stale run directory survived startup: %v", err)
	}
}

func TestEnableExecutionRejectsAnInvalidLimit(t *testing.T) {
	rig := newAvailableInterpreterRig(t)
	invalid := codeexec.Default()
	invalid.MaxProcesses = 0
	mod, err := codeexec.New(t.Context(), &codeexec.ModuleDeps{
		Config:    &invalid,
		DataRoot:  rig.dataRoot,
		Manifest:  rig.manifest,
		Artifacts: testArtifacts(t),
		Logger:    slog.New(slog.DiscardHandler),
	})
	if err == nil {
		t.Fatal("codeexec.New accepted a zero process limit")
	}
	if mod != nil {
		t.Fatal("a rejected configuration returned a non-nil module")
	}
}

func TestHealthReasonForAFailedProbeMatchesTheVerifierReason(t *testing.T) {
	if codeexec.ReasonProbeFailed != string(interpreter.ReasonProbeFailed) {
		t.Fatalf("health reason %q differs from the verifier reason %q", codeexec.ReasonProbeFailed, interpreter.ReasonProbeFailed)
	}
	if codeexec.RuntimeStateInvalid != string(interpreter.StateInvalid) {
		t.Fatalf("health invalid state %q differs from %q", codeexec.RuntimeStateInvalid, interpreter.StateInvalid)
	}
}

func testArtifacts(t *testing.T) storage.ArtifactStore {
	t.Helper()
	return storage.NewArtifactStore(sqlitetest.OpenMigrated(t, Schemas()...), t.TempDir(), storage.DefaultArtifactQuotaBytes)
}
