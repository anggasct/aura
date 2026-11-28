package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const webOnlyBase = "version: 1\ntools:\n  workspace: /srv/aura/workspace\nskills:\n  roots: [/srv/aura/skills]\ncontext:\n  recent_complete_turns: 5\nprofile:\n  prompt_version: v1\nchildren:\n  recovery: interrupt\nvision: {}\n"

func TestWebOnlyCapsConfig(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		result, err := LoadWithOptions(writeTempConfig(t, webOnlyBase), execLinuxOptions(t))
		if err != nil {
			t.Fatalf("LoadWithOptions: %v", err)
		}
		if got := result.Config.Tools; got.Fetch.WebOnlyFetchesPerRun != 10 || got.WebSearch.WebOnlyQueriesPerRun != 5 {
			t.Errorf("caps = %d fetches and %d queries, want 10 and 5", got.Fetch.WebOnlyFetchesPerRun, got.WebSearch.WebOnlyQueriesPerRun)
		}
	})
	t.Run("the file and the environment", func(t *testing.T) {
		t.Setenv("AURA_TOOLS_WEB_SEARCH_WEB_ONLY_QUERIES_PER_RUN", "20")
		content := strings.Replace(webOnlyBase, "tools:\n", "tools:\n  fetch:\n    web_only_fetches_per_run: 3\n", 1)
		result, err := LoadWithOptions(writeTempConfig(t, content), execLinuxOptions(t))
		if err != nil {
			t.Fatalf("LoadWithOptions: %v", err)
		}
		if got := result.Config.Tools; got.Fetch.WebOnlyFetchesPerRun != 3 || got.WebSearch.WebOnlyQueriesPerRun != 20 {
			t.Errorf("caps = %d fetches and %d queries, want 3 and 20", got.Fetch.WebOnlyFetchesPerRun, got.WebSearch.WebOnlyQueriesPerRun)
		}
		t.Setenv("AURA_TOOLS_FETCH_WEB_ONLY_FETCHES_PER_RUN", "50")
		result, err = LoadWithOptions(writeTempConfig(t, content), execLinuxOptions(t))
		if err != nil || result.Config.Tools.Fetch.WebOnlyFetchesPerRun != 50 {
			t.Errorf("the environment did not override the file: %+v, %v", result.Config.Tools, err)
		}
	})
	t.Run("zero turns the relief off", func(t *testing.T) {
		t.Setenv("AURA_TOOLS_FETCH_WEB_ONLY_FETCHES_PER_RUN", "0")
		t.Setenv("AURA_TOOLS_WEB_SEARCH_WEB_ONLY_QUERIES_PER_RUN", "0")
		result, err := LoadWithOptions(writeTempConfig(t, webOnlyBase), execLinuxOptions(t))
		if err != nil || result.Config.Tools.Fetch.WebOnlyFetchesPerRun != 0 || result.Config.Tools.WebSearch.WebOnlyQueriesPerRun != 0 {
			t.Errorf("caps = %+v, %v, want zero", result.Config.Tools, err)
		}
	})
	for name, tc := range map[string]struct{ env, value, path string }{
		"fetches above the bound": {"AURA_TOOLS_FETCH_WEB_ONLY_FETCHES_PER_RUN", "51", "tools.fetch.web_only_fetches_per_run"},
		"fetches below zero":      {"AURA_TOOLS_FETCH_WEB_ONLY_FETCHES_PER_RUN", "-1", "tools.fetch.web_only_fetches_per_run"},
		"queries above the bound": {"AURA_TOOLS_WEB_SEARCH_WEB_ONLY_QUERIES_PER_RUN", "21", "tools.web_search.web_only_queries_per_run"},
		"queries below zero":      {"AURA_TOOLS_WEB_SEARCH_WEB_ONLY_QUERIES_PER_RUN", "-1", "tools.web_search.web_only_queries_per_run"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(tc.env, tc.value)
			_, err := LoadWithOptions(writeTempConfig(t, webOnlyBase), execLinuxOptions(t))
			if err == nil || !strings.Contains(err.Error(), "config_invalid") || !strings.Contains(err.Error(), tc.path) {
				t.Errorf("err = %v, want config_invalid at %s", err, tc.path)
			}
		})
	}
}

func TestWorkspaceAndDataDirMustNotNest(t *testing.T) {
	root := t.TempDir()
	inside := filepath.Join(root, "data", "workspace")
	if err := os.MkdirAll(inside, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link-to-data")
	if err := os.Symlink(filepath.Join(root, "data"), link); err != nil {
		t.Fatal(err)
	}
	sibling := filepath.Join(root, "workspace")
	cases := map[string]struct {
		data, workspace string
		nested          bool
	}{
		"the same directory":                                    {filepath.Join(root, "data"), filepath.Join(root, "data"), true},
		"a workspace inside the data":                           {filepath.Join(root, "data"), inside, true},
		"a data directory inside the workspace":                 {filepath.Join(root, "data"), root, true},
		"a workspace reached by a link":                         {filepath.Join(root, "data"), filepath.Join(link, "workspace"), true},
		"a data directory not yet created inside the workspace": {filepath.Join(root, "new", "data"), filepath.Join(root, "new"), true},
		"siblings":                         {filepath.Join(root, "data"), sibling, false},
		"a name that only shares a prefix": {filepath.Join(root, "data"), filepath.Join(root, "data-workspace"), false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			content := strings.Replace(webOnlyBase, "workspace: /srv/aura/workspace", "workspace: "+tc.workspace, 1) + "storage:\n  path: " + tc.data + "\n"
			_, err := LoadWithOptions(writeTempConfig(t, content), execLinuxOptions(t))
			if tc.nested != (err != nil && strings.Contains(err.Error(), "tools.workspace") && strings.Contains(err.Error(), "config_invalid")) {
				t.Errorf("err = %v, want nested=%t", err, tc.nested)
			}
			if !tc.nested && err != nil {
				t.Errorf("a valid layout was refused: %v", err)
			}
		})
	}
}
