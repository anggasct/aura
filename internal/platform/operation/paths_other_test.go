//go:build !linux

package operation_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/operation"
	"github.com/anggasct/aura/internal/platform/workspace"
)

func workspacePathResolutionCases() []pathResolutionCase {
	return nil
}

func TestTheKernelRefusesWorkspacePathsWithoutLinux(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "policy.md"), []byte("workspace body"), 0o600); err != nil {
		t.Fatal(err)
	}
	var seen pathObservation
	rig := pathRig(t, dir, &seen)

	_, err := rig.Registry.Invoke(t.Context(), filesRead, cliPathCall(operation.SurfaceCLI, `{"source":"workspace:policy.md"}`))

	if !fault.HasCode(err, operation.ErrorCodeSourceUnavailable) || !fault.HasCode(err, workspace.ErrorCodeUnsupported) {
		t.Fatalf("Invoke = %v, want %s wrapping %s", err, operation.ErrorCodeSourceUnavailable, workspace.ErrorCodeUnsupported)
	}
	if seen.HasFile {
		t.Error("the handler received a file although workspace access is unsupported")
	}
}
