package config

import (
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/capability"
)

func execLinuxOptions(t *testing.T) LoadOptions {
	t.Helper()
	build, err := capability.NewBuild(string(capability.ProfileExecLinux), nil, "linux")
	if err != nil {
		t.Fatalf("NewBuild: %v", err)
	}
	return LoadOptions{Build: build, Registry: capability.EmptyRegistry()}
}

func TestToolEnabledProfileRequiresToolsSection(t *testing.T) {
	_, err := LoadWithOptions(writeTempConfig(t, "version: 1\n"), execLinuxOptions(t))
	if err == nil || !strings.Contains(err.Error(), "tools section is required") {
		t.Fatalf("LoadWithOptions error = %v", err)
	}
}

func TestToolConfigAppliesDefaultsAndEnvironmentOverrides(t *testing.T) {
	t.Setenv("AURA_TOOLS_MAX_INLINE_RESULT_BYTES", "8192")
	t.Setenv("AURA_TOOLS_WEB_SEARCH_MAX_RESULTS", "3")
	result, err := LoadWithOptions(writeTempConfig(t, "version: 1\ntools:\n  workspace: /srv/aura/workspace\n"), execLinuxOptions(t))
	if err != nil {
		t.Fatalf("LoadWithOptions: %v", err)
	}
	if result.Config.Tools == nil {
		t.Fatal("Tools = nil")
	}
	if result.Config.Tools.MaxInlineResultBytes != 8192 || result.Config.Tools.WebSearch.MaxResults != 3 {
		t.Fatalf("Tools = %+v", result.Config.Tools)
	}
	if result.Config.Tools.Fetch.MaxDecodedBytes <= 0 || result.Config.Tools.Exec.Timeout <= 0 {
		t.Fatalf("tool defaults were not applied: %+v", result.Config.Tools)
	}
}
