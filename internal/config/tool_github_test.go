package config

import (
	"errors"
	"fmt"
	"runtime"
	"strings"
	"testing"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/anggasct/aura/internal/platform/capability"
)

const githubCapabilityEnabled = "capabilities:\n  enabled: [provider-github]\n"

func loadWithGitHubCapability(t testing.TB, path string) (LoadResult, error) {
	t.Helper()
	return loadUnderBuild(t, path, "provider-github")
}

func loadUnderBuild(t testing.TB, path, compiled string) (LoadResult, error) {
	t.Helper()
	build, err := capability.ParseBuild("core", compiled, runtime.GOOS)
	if err != nil {
		t.Fatalf("ParseBuild: %v", err)
	}
	registry, err := capability.BuiltinRegistry()
	if err != nil {
		t.Fatalf("BuiltinRegistry: %v", err)
	}
	return LoadWithOptions(path, LoadOptions{Build: build, Registry: registry})
}

func githubConfigSection(entries string) string {
	return "  github:\n    credentials:\n" + entries
}

func githubEntry(name, ref string) string {
	return fmt.Sprintf("      - name: %q\n        ref: %q\n", name, ref)
}

func loadGitHubConfig(t *testing.T, section string) (*Tools, error) {
	t.Helper()
	body := strings.Replace(webSearchBase, "%s\n", section, 1)
	result, err := LoadWithOptions(writeTempConfig(t, body), execLinuxOptions(t))
	if err != nil {
		return nil, err
	}
	return result.Config.Tools, nil
}

func TestToolsGitHubCredentialsLoadInOrder(t *testing.T) {
	loaded, err := loadGitHubConfig(t, "")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(loaded.GitHub.Credentials) != 0 || len(Default().Tools.GitHub.Credentials) != 0 {
		t.Fatalf("GitHub = %+v, default %+v, want no credentials", loaded.GitHub, Default().Tools.GitHub)
	}
	section := githubConfigSection(githubEntry("ci", "env://GITHUB_TOKEN") + githubEntry("release", "file:///etc/aura/github-release-token"))
	body := strings.Replace(webSearchBase, "%s\n", section, 1) + githubCapabilityEnabled
	result, err := loadWithGitHubCapability(t, writeTempConfig(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	loaded = result.Config.Tools
	want := []GitHubCredential{{Name: "ci", Ref: "env://GITHUB_TOKEN"}, {Name: "release", Ref: "file:///etc/aura/github-release-token"}}
	if got := loaded.GitHub.Credentials; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("Credentials = %+v, want %+v", got, want)
	}
	marshaled, err := yamlv3.Marshal(Default())
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(marshaled), "github") {
		t.Errorf("Default() marshals a github key:\n%s", marshaled)
	}
}

func TestToolsGitHubRejectsUnusableCredentialValues(t *testing.T) {
	const nameDetail = "tools.github.credentials[%d].name must be 1 to 64 lowercase letters, digits, '_' or '-', starting with a letter"
	const refDetail = "tools.github.credentials[%d].ref must name an environment variable or an absolute, clean file path"
	cases := []struct {
		name    string
		entries string
		detail  string
	}{
		{"empty name", githubEntry("", "env://A"), fmt.Sprintf(nameDetail, 0)},
		{"uppercase name", githubEntry("CI", "env://A"), fmt.Sprintf(nameDetail, 0)},
		{"dotted name", githubEntry("c.i", "env://A"), fmt.Sprintf(nameDetail, 0)},
		{"long name", githubEntry("a"+strings.Repeat("b", 64), "env://A"), fmt.Sprintf(nameDetail, 0)},
		{"repeated name", githubEntry("ci", "env://A") + githubEntry("ci", "env://B"), "tools.github.credentials[1].name repeats an earlier entry"},
		{"literal token", githubEntry("ci", "ghp_literal"), fmt.Sprintf(refDetail, 0)},
		{"empty env name", githubEntry("ci", "env://"), fmt.Sprintf(refDetail, 0)},
		{"digit env name", githubEntry("ci", "env://1BAD"), fmt.Sprintf(refDetail, 0)},
		{"spaced env name", githubEntry("ci", "env://A B"), fmt.Sprintf(refDetail, 0)},
		{"empty file path", githubEntry("ci", "file://"), fmt.Sprintf(refDetail, 0)},
		{"relative file path", githubEntry("ci", "file://relative/token"), fmt.Sprintf(refDetail, 0)},
		{"unclean file path", githubEntry("ci", "file:///a/../b"), fmt.Sprintf(refDetail, 0)},
		{"trailing slash", githubEntry("ci", "file:///a/"), fmt.Sprintf(refDetail, 0)},
		{"repeated ref", githubEntry("ci", "env://A") + githubEntry("release", "env://A"), "tools.github.credentials[1].ref repeats an earlier entry"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadGitHubConfig(t, githubConfigSection(tc.entries))
			var coded *Error
			if !errors.As(err, &coded) || coded.Code != ErrorCodeConfigInvalid || coded.Detail != tc.detail {
				t.Fatalf("err = %v, want config_invalid %q", err, tc.detail)
			}
			for _, leaked := range []string{"env:", "file:", "ghp_literal", "relative/token", "/a/"} {
				if strings.Contains(err.Error(), leaked) {
					t.Errorf("error %q contains %q", err, leaked)
				}
			}
		})
	}
}

func TestToolsGitHubRejectsMisshapenCredentials(t *testing.T) {
	cases := []struct {
		name    string
		section string
		want    string
	}{
		{"scalar github", "  github: 5\n", "tools.github must be a mapping"},
		{"scalar credentials", "  github:\n    credentials: ci\n", "tools.github.credentials must be a sequence"},
		{"scalar entry", "  github:\n    credentials:\n      - ci\n", "tools.github.credentials[0] must be a mapping"},
		{"numeric name", "  github:\n    credentials:\n      - name: 5\n        ref: env://A\n", "tools.github.credentials[0].name must be a string"},
		{"numeric ref", "  github:\n    credentials:\n      - name: ci\n        ref: 5\n", "tools.github.credentials[0].ref must be a string"},
		{"old key", "  github:\n    credentials:\n      - name: ci\n        credential_ref: env://A\n", `unknown key "tools.github.credentials.credential_ref"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadGitHubConfig(t, tc.section)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to say %q", err, tc.want)
			}
		})
	}
}

func TestOwnerPreferencesMayNotRepeatAGitHubCredential(t *testing.T) {
	const token = "ghp-owner-check-token"
	t.Setenv("AURA_TEST_GITHUB_TOKEN", token)
	body := strings.Replace(webSearchBase, "%s\n", githubConfigSection(githubEntry("ci", "env://AURA_TEST_GITHUB_TOKEN")), 1)
	_, err := LoadWithOptions(writeTempConfig(t, body+"owner:\n  preferences:\n    note: \""+token+"\"\n"), execLinuxOptions(t))
	code, ok := CodeOf(err)
	if !ok || code != ErrorCodeOwnerSecret {
		t.Fatalf("code = %v, %v, want owner_secret_invalid (err %v)", code, ok, err)
	}
	if strings.Contains(err.Error(), token) {
		t.Errorf("error leaks the token: %v", err)
	}
}

func TestToolsGitHubCredentialsNeedTheProviderCapability(t *testing.T) {
	const detail = "tools.github.credentials needs the provider-github capability: compile it into the build and list it in capabilities.enabled"
	credentials := strings.Replace(webSearchBase, "%s\n", githubConfigSection(githubEntry("ci", "env://GITHUB_TOKEN")), 1)
	none := strings.Replace(webSearchBase, "%s\n", "", 1)
	cases := []struct {
		name     string
		body     string
		compiled string
		refused  bool
	}{
		{"compiled and enabled", credentials + githubCapabilityEnabled, "provider-github", false},
		{"compiled, not enabled", credentials, "provider-github", true},
		{"enabled, not compiled", credentials + githubCapabilityEnabled, "", true},
		{"no credentials, no capability", none, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadUnderBuild(t, writeTempConfig(t, tc.body), tc.compiled)
			if !tc.refused {
				if err != nil {
					t.Fatalf("Load: %v", err)
				}
				return
			}
			var coded *Error
			if !errors.As(err, &coded) || coded.Code != ErrorCodeConfigInvalid || coded.Detail != detail {
				t.Fatalf("err = %v, want config_invalid with %q", err, detail)
			}
		})
	}
}
