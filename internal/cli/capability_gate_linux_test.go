//go:build linux

package cli

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/modules/workflow"
	"github.com/anggasct/aura/internal/platform/capability"
)

const allCapabilities = "exec-linux,workspace-read,workspace-write,public-web,provider-search"

func loadForBuild(t *testing.T, profile, compiled string, enabled []string) config.LoadResult {
	t.Helper()
	registry, err := capability.BuiltinRegistry()
	if err != nil {
		t.Fatalf("BuiltinRegistry: %v", err)
	}
	build, err := capability.ParseBuild(profile, compiled, "linux")
	if err != nil {
		t.Fatalf("ParseBuild: %v", err)
	}
	return loadCapabilityConfig(t, enabled, config.LoadOptions{
		Build:    build,
		Registry: registry,
		Dependencies: capability.Dependencies{
			capability.DependencyProcessContainment: true,
			capability.DependencyInterpreterRuntime: true,
		},
	})
}

func TestWorkflowToolRunnerFollowsTheHostCapabilitySet(t *testing.T) {
	cases := []struct {
		name       string
		profile    string
		compiled   string
		enabled    []string
		tool       string
		arguments  string
		effect     string
		wantDenied bool
	}{
		{
			name: "write_file with workspace-write enabled", profile: "exec-linux", compiled: allCapabilities,
			enabled: []string{"workspace-write"}, tool: "write_file", arguments: `{"path":"out.txt","content":"x"}`, effect: "out.txt",
		},
		{
			name: "write_file with nothing enabled", profile: "exec-linux", compiled: allCapabilities,
			tool: "write_file", arguments: `{"path":"out.txt","content":"x"}`, effect: "out.txt", wantDenied: true,
		},
		{
			name: "exec with only workspace-write enabled", profile: "exec-linux", compiled: allCapabilities,
			enabled: []string{"workspace-write"}, tool: "exec", arguments: `{"command":["touch","marker"]}`, effect: "marker", wantDenied: true,
		},
		{
			name: "exec on a core build that lists exec-linux", profile: "core", compiled: "",
			enabled: []string{"exec-linux"}, tool: "exec", arguments: `{"command":["touch","marker"]}`, effect: "marker", wantDenied: true,
		},
		{
			name: "write_file on a core build that lists workspace-write", profile: "core", compiled: "",
			enabled: []string{"workspace-write"}, tool: "write_file", arguments: `{"path":"out.txt","content":"x"}`, effect: "out.txt", wantDenied: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := loadForBuild(t, tc.profile, tc.compiled, tc.enabled)
			db, err := openStorage(t.Context(), result.Config)
			if err != nil {
				t.Fatalf("openStorage: %v", err)
			}
			t.Cleanup(func() { _ = db.Close() })
			runner, err := newWorkflowToolRunner(&result, db, slog.New(slog.DiscardHandler))
			if err != nil {
				t.Fatalf("newWorkflowToolRunner: %v", err)
			}
			_, invokeErr := runner.Invoke(t.Context(), tc.tool, json.RawMessage(tc.arguments))
			_, statErr := os.Stat(filepath.Join(result.Config.Tools.Workspace, tc.effect))
			if tc.wantDenied {
				if code, ok := workflow.CodeOf(invokeErr); !ok || code != workflow.ErrorCodeCapabilityUnavailable {
					t.Fatalf("Invoke error = %v, want capability_unavailable", invokeErr)
				}
				if statErr == nil {
					t.Fatalf("the denied call left %s in the workspace", tc.effect)
				}
				return
			}
			if invokeErr != nil {
				t.Fatalf("Invoke: %v", invokeErr)
			}
			if statErr != nil {
				t.Fatalf("the allowed call left no effect: %v", statErr)
			}
		})
	}
}

func TestCoreBuildListingExecLinuxReportsAStateErrorTheWorkflowCommandsIgnore(t *testing.T) {
	result := loadForBuild(t, "core", "", []string{"exec-linux"})
	if result.CapabilityStateError == nil {
		t.Fatal("a core build listing exec-linux reported no capability state error")
	}
	if active := result.CapabilityReport.Active(); len(active) != 0 {
		t.Fatalf("active capabilities = %v, want none", active)
	}
}
