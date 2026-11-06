package config

import (
	"testing"

	"github.com/anggasct/aura/internal/capability"
	"github.com/anggasct/aura/internal/tools/interpreter"
)

func TestLoad_CodeInterpreterCapabilityByBuildAndPlatform(t *testing.T) {
	registry, err := capability.BuiltinRegistry()
	if err != nil {
		t.Fatal(err)
	}
	contained := capability.Dependencies{capability.DependencyProcessContainment: true}
	base := "version: 1\ntools:\n  workspace: /srv/aura/workspace\nskills:\n  roots: [/srv/aura/skills]\ncontext:\n  recent_complete_turns: 5\nprofile:\n  prompt_version: v1\nchildren:\n  recovery: interrupt\nvision: {}\ncapabilities:\n  enabled: [code-interpreter]\n"
	cases := []struct {
		name          string
		profile       string
		compiled      []string
		goarch        string
		wantCode      capability.ErrorCode
		wantAvailable bool
	}{
		{"exec-linux on amd64", "exec-linux", []string{"code-interpreter"}, "amd64", "", true},
		{"native-linux on amd64", "native-linux", []string{"code-interpreter"}, "amd64", "", true},
		{"exec-linux on arm64", "exec-linux", []string{"code-interpreter"}, "arm64", capability.ErrorCodeDependencyMissing, false},
		{"exec-linux without the capability compiled", "exec-linux", nil, "amd64", capability.ErrorCodeCapabilityUnavailable, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			build, err := capability.NewBuild(tc.profile, tc.compiled, "linux")
			if err != nil {
				t.Fatal(err)
			}
			options := LoadOptions{Build: build, Registry: registry, Dependencies: interpreter.Dependencies("linux", tc.goarch, contained)}
			result, err := LoadWithOptions(writeTempConfig(t, base), options)
			if err != nil {
				t.Fatalf("LoadWithOptions: %v", err)
			}
			if tc.wantCode == "" {
				if result.CapabilityStateError != nil {
					t.Fatalf("CapabilityStateError = %v", result.CapabilityStateError)
				}
			} else if code, ok := capability.CodeOf(result.CapabilityStateError); !ok || code != tc.wantCode {
				t.Fatalf("CodeOf(%v) = %q, %v; want %q", result.CapabilityStateError, code, ok, tc.wantCode)
			}
			status, _ := result.CapabilityReport.Status("code-interpreter")
			if status.Available != tc.wantAvailable {
				t.Errorf("status = %+v, want available=%v", status, tc.wantAvailable)
			}
		})
	}
}

func TestLoad_CodeInterpreterIsExcludedFromTheCoreProfile(t *testing.T) {
	registry, err := capability.BuiltinRegistry()
	if err != nil {
		t.Fatal(err)
	}
	build, err := capability.NewBuild("core", nil, "linux")
	if err != nil {
		t.Fatal(err)
	}
	options := LoadOptions{Build: build, Registry: registry, Dependencies: interpreter.Dependencies("linux", "amd64", capability.Dependencies{capability.DependencyProcessContainment: true})}
	result, err := LoadWithOptions(writeTempConfig(t, "version: 1\ncapabilities:\n  enabled: [code-interpreter]\n"), options)
	if err != nil {
		t.Fatalf("LoadWithOptions: %v", err)
	}
	if code, ok := capability.CodeOf(result.CapabilityStateError); !ok || code != capability.ErrorCodeCapabilityUnavailable {
		t.Fatalf("CodeOf(%v) = %q, %v; want capability_unavailable", result.CapabilityStateError, code, ok)
	}
	status, ok := result.CapabilityReport.Status("code-interpreter")
	if !ok || status.Compiled || status.Available || !status.Enabled {
		t.Errorf("status = %+v, %v; want enabled, not compiled and unavailable", status, ok)
	}
}
