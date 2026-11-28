//go:build !linux

package operation

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/workspace"
)

func workspaceAuthorityCases(dir string, agent *surfaceFiles) []fileAuthorityCase {
	return []fileAuthorityCase{
		{name: "a workspace file", files: agent, source: "policy.md", wantCode: ErrorCodeSourceUnavailable},
		{name: "an escaping path", files: agent, source: "../policy.md", wantCode: ErrorCodeSourceUnavailable},
		{name: "an absolute path", files: agent, source: filepath.Join(dir, "policy.md"), wantCode: ErrorCodeSourceUnavailable},
		{name: "a missing file", files: agent, source: "missing.md", wantCode: ErrorCodeSourceUnavailable},
		{name: "a directory", files: agent, source: "directory", wantCode: ErrorCodeSourceUnavailable},
	}
}

func TestWorkspaceFilesFailClosedWithoutLinux(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "policy.md"), []byte("policy body"), 0o600); err != nil {
		t.Fatal(err)
	}
	agent := &surfaceFiles{deps: &FileDeps{Workspace: dir}, ownerOnly: true, sessionID: "session-1"}

	for _, source := range []string{"policy.md", "../policy.md", filepath.Join(dir, "policy.md")} {
		t.Run(source, func(t *testing.T) {
			file, err := agent.Open(t.Context(), source)
			if file != nil {
				_ = file.Close()
			}
			if !fault.HasCode(err, ErrorCodeSourceUnavailable) || !fault.HasCode(err, workspace.ErrorCodeUnsupported) {
				t.Fatalf("Open(%q) = %v, want %s wrapping %s", source, err, ErrorCodeSourceUnavailable, workspace.ErrorCodeUnsupported)
			}
			if strings.Contains(err.Error(), dir) {
				t.Errorf("error %q names the workspace path", err)
			}
		})
	}
}
