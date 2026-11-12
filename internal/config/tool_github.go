package config

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	yamlv3 "gopkg.in/yaml.v3"
)

var githubCredentialNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)

var githubEnvNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

type ToolGitHub struct {
	Credentials []GitHubCredential `koanf:"credentials" yaml:"credentials,omitempty"`
}

type GitHubCredential struct {
	Name string `koanf:"name" yaml:"name"`
	Ref  string `koanf:"ref" yaml:"ref"`
}

func validateToolGitHubShape(node *yamlv3.Node) error {
	if node.Kind != yamlv3.MappingNode {
		return fmt.Errorf("tools.github must be a mapping at line %d", node.Line)
	}
	list := mappingValue(node, "credentials")
	if list == nil {
		return nil
	}
	if list.Kind != yamlv3.SequenceNode {
		return fmt.Errorf("tools.github.credentials must be a sequence at line %d", list.Line)
	}
	for index, entry := range list.Content {
		if entry.Kind != yamlv3.MappingNode {
			return fmt.Errorf("tools.github.credentials[%d] must be a mapping at line %d", index, entry.Line)
		}
		for _, field := range []string{"name", "ref"} {
			if value := mappingValue(entry, field); value != nil && (value.Kind != yamlv3.ScalarNode || value.Tag != "!!str") {
				return fmt.Errorf("tools.github.credentials[%d].%s must be a string at line %d", index, field, value.Line)
			}
		}
	}
	return nil
}

func validateToolGitHub(github *ToolGitHub) error {
	names := make(map[string]bool, len(github.Credentials))
	refs := make(map[string]bool, len(github.Credentials))
	for i := range github.Credentials {
		entry := &github.Credentials[i]
		path := fmt.Sprintf("tools.github.credentials[%d]", i)
		detail := ""
		switch {
		case !githubCredentialNamePattern.MatchString(entry.Name):
			detail = path + ".name must be 1 to 64 lowercase letters, digits, '_' or '-', starting with a letter"
		case names[entry.Name]:
			detail = path + ".name repeats an earlier entry"
		case !validGitHubCredentialRef(entry.Ref):
			detail = path + ".ref must name an environment variable or an absolute, clean file path"
		case refs[entry.Ref]:
			detail = path + ".ref repeats an earlier entry"
		}
		if detail != "" {
			return &Error{Code: ErrorCodeConfigInvalid, Detail: detail}
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
