package config

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/anggasct/aura/internal/platform/configkit"
)

var githubCredentialNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)

var githubEnvNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

type ToolGitHub struct {
	Credentials []GitHubCredential `json:"credentials,omitempty"`
}

type GitHubCredential struct {
	Name string `json:"name"`
	Ref  string `json:"ref"`
}

func validateToolGitHub(github *ToolGitHub) error {
	names := make(map[string]bool, len(github.Credentials))
	refs := make(map[string]bool, len(github.Credentials))
	for i := range github.Credentials {
		entry := &github.Credentials[i]
		path := fmt.Sprintf("tools.github.credentials[%d]", i)
		var problem error
		switch {
		case !githubCredentialNamePattern.MatchString(entry.Name):
			problem = configkit.Invalid(path+".name", "must be 1 to 64 lowercase letters, digits, '_' or '-', starting with a letter")
		case names[entry.Name]:
			problem = configkit.Invalid(path+".name", "repeats an earlier entry")
		case !validGitHubCredentialRef(entry.Ref):
			problem = configkit.Invalid(path+".ref", "must name an environment variable or an absolute, clean file path")
		case refs[entry.Ref]:
			problem = configkit.Invalid(path+".ref", "repeats an earlier entry")
		}
		if problem != nil {
			return problem
		}
		names[entry.Name] = true
		refs[entry.Ref] = true
	}
	return nil
}

func validGitHubCredentialRef(ref string) bool {
	if name, ok := strings.CutPrefix(ref, "env://"); ok {
		return githubEnvNamePattern.MatchString(name)
	}
	if path, ok := strings.CutPrefix(ref, "file://"); ok {
		return filepath.IsAbs(path) && filepath.Clean(path) == path
	}
	return false
}
