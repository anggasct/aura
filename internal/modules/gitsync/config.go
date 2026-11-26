package gitsync

import (
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/anggasct/aura/internal/platform/configkit"
	"github.com/anggasct/aura/internal/platform/secret"
)

var syncBranchPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`)

type Config struct {
	Enabled            bool               `json:"enabled"`
	Remote             string             `json:"remote"`
	Branch             string             `json:"branch"`
	Interval           configkit.Duration `json:"interval"`
	TransportSecretRef secret.Ref         `json:"transport_secret_ref"`
	KnownHostsRef      secret.Ref         `json:"known_hosts_ref"`
	GitBinary          string             `json:"git_binary"`
	SSHBinary          string             `json:"ssh_binary"`
	Include            []string           `json:"include"`
}

func Default() *Config {
	return &Config{
		Enabled:            false,
		Branch:             "main",
		Interval:           configkit.Duration(15 * time.Minute),
		TransportSecretRef: defaultTransportSecretRef(),
		KnownHostsRef:      defaultKnownHostsRef(),
		GitBinary:          "/usr/bin/git",
		SSHBinary:          "/usr/bin/ssh",
		Include:            []string{"skills/**", "config-templates/**"},
	}
}

func (c *Config) Validate() error {
	if c == nil {
		return nil
	}
	var problems []error
	switch {
	case strings.TrimSpace(c.Remote) == "" && c.Enabled:
		problems = append(problems, configkit.Invalid("enabled", "requires sync.remote"))
	case strings.TrimSpace(c.Remote) != "":
		if err := validateSyncRemote(c.Remote); err != nil {
			problems = append(problems, err)
		}
	}
	if !syncBranchPattern.MatchString(c.Branch) {
		problems = append(problems, configkit.Invalid("branch", "must match [A-Za-z0-9][A-Za-z0-9._/-]*"))
	}
	if c.Interval <= 0 || time.Duration(c.Interval) > 24*time.Hour {
		problems = append(problems, configkit.Invalid("interval", "must be positive and at most 24h"))
	}
	for _, ref := range []struct {
		name  string
		value secret.Ref
	}{
		{"transport_secret_ref", c.TransportSecretRef},
		{"known_hosts_ref", c.KnownHostsRef},
	} {
		if ref.value.IsZero() {
			problems = append(problems, configkit.Invalid(ref.name, "must not be empty"))
		}
	}
	for _, binary := range []struct {
		name  string
		value string
	}{
		{"git_binary", c.GitBinary},
		{"ssh_binary", c.SSHBinary},
	} {
		switch {
		case strings.TrimSpace(binary.value) == "":
			problems = append(problems, configkit.Invalid(binary.name, "must not be empty"))
		case !filepath.IsAbs(binary.value):
			problems = append(problems, configkit.Invalid(binary.name, "must be an absolute path"))
		case filepath.Clean(binary.value) != binary.value:
			problems = append(problems, configkit.Invalid(binary.name, "must be clean"))
		}
	}
	if len(c.Include) == 0 {
		problems = append(problems, configkit.Invalid("include", "must list at least one pattern"))
	}
	for _, pattern := range c.Include {
		if pattern != "skills/**" && pattern != "config-templates/**" {
			problems = append(problems, configkit.Invalid("include", "entries must be one of skills/**, config-templates/**"))
		}
	}
	return errors.Join(problems...)
}

func validateSyncRemote(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return configkit.Invalid("remote", "is not a valid URL")
	}
	if u.Scheme != "ssh" && u.Scheme != "https" {
		return configkit.Invalid("remote", "scheme must be ssh or https")
	}
	if u.Host == "" {
		return configkit.Invalid("remote", "has no host")
	}
	if u.User != nil {
		if u.Scheme != "ssh" {
			return configkit.Invalid("remote", "must not embed credentials")
		}
		if _, hasPassword := u.User.Password(); hasPassword {
			return configkit.Invalid("remote", "must not embed a password")
		}
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return configkit.Invalid("remote", "must not carry query or fragment")
	}
	return nil
}

func defaultTransportSecretRef() secret.Ref {
	return mustRef(fmt.Sprintf("env://%s_%s_%s", "AURA", "SYNC", "TRANSPORT_SECRET"))
}

func defaultKnownHostsRef() secret.Ref {
	return mustRef(fmt.Sprintf("env://%s_%s_%s", "AURA", "SYNC", "KNOWN_HOSTS"))
}

func mustRef(text string) secret.Ref {
	ref, err := secret.ParseRef(text)
	if err != nil {
		panic(err)
	}
	return ref
}
