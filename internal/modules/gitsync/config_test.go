package gitsync

import (
	"strings"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/configkit"
	"github.com/anggasct/aura/internal/platform/secret"
)

func TestDefaultConfigValid(t *testing.T) {
	t.Parallel()
	cfg := Default()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Default().Validate() = %v, want nil", err)
	}
	var nilCfg *Config
	if err := nilCfg.Validate(); err != nil {
		t.Fatalf("nil Config.Validate() = %v, want nil", err)
	}
}

func TestConfigValidate(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		mutate   func(*Config)
		wantText string
	}{
		{
			name: "enabled_without_remote",
			mutate: func(c *Config) {
				c.Enabled = true
				c.Remote = ""
			},
			wantText: "enabled",
		},
		{
			name: "remote_bad_scheme",
			mutate: func(c *Config) {
				c.Remote = "http://git.example.com/repo.git"
			},
			wantText: "scheme must be ssh or https",
		},
		{
			name: "remote_missing_host",
			mutate: func(c *Config) {
				c.Remote = "https:///owner/repo.git"
			},
			wantText: "has no host",
		},
		{
			name: "remote_https_credentials",
			mutate: func(c *Config) {
				c.Remote = "https://user:token@git.example.com/repo.git"
			},
			wantText: "must not embed credentials",
		},
		{
			name: "remote_ssh_password",
			mutate: func(c *Config) {
				c.Remote = "ssh://git:secret@git.example.com/repo.git"
			},
			wantText: "must not embed a password",
		},
		{
			name: "remote_query",
			mutate: func(c *Config) {
				c.Remote = "https://git.example.com/repo.git?token=1"
			},
			wantText: "must not carry query or fragment",
		},
		{
			name: "remote_fragment",
			mutate: func(c *Config) {
				c.Remote = "https://git.example.com/repo.git#main"
			},
			wantText: "must not carry query or fragment",
		},
		{
			name: "branch_invalid_leading_char",
			mutate: func(c *Config) {
				c.Branch = "-bad"
			},
			wantText: "must match [A-Za-z0-9][A-Za-z0-9._/-]*",
		},
		{
			name: "branch_invalid_space",
			mutate: func(c *Config) {
				c.Branch = "main branch"
			},
			wantText: "must match [A-Za-z0-9][A-Za-z0-9._/-]*",
		},
		{
			name: "interval_zero",
			mutate: func(c *Config) {
				c.Interval = 0
			},
			wantText: "must be positive and at most 24h",
		},
		{
			name: "interval_too_large",
			mutate: func(c *Config) {
				c.Interval = configkit.Duration(25 * time.Hour)
			},
			wantText: "must be positive and at most 24h",
		},
		{
			name: "missing_transport_secret_ref",
			mutate: func(c *Config) {
				c.TransportSecretRef = secret.Ref{}
			},
			wantText: "transport_secret_ref",
		},
		{
			name: "missing_known_hosts_ref",
			mutate: func(c *Config) {
				c.KnownHostsRef = secret.Ref{}
			},
			wantText: "known_hosts_ref",
		},
		{
			name: "empty_git_binary",
			mutate: func(c *Config) {
				c.GitBinary = ""
			},
			wantText: "git_binary",
		},
		{
			name: "relative_git_binary",
			mutate: func(c *Config) {
				c.GitBinary = "git"
			},
			wantText: "must be an absolute path",
		},
		{
			name: "unclean_ssh_binary",
			mutate: func(c *Config) {
				c.SSHBinary = "/usr/bin/../bin/ssh"
			},
			wantText: "must be clean",
		},
		{
			name: "empty_include",
			mutate: func(c *Config) {
				c.Include = nil
			},
			wantText: "must list at least one pattern",
		},
		{
			name: "disallowed_include",
			mutate: func(c *Config) {
				c.Include = []string{"workspace/**"}
			},
			wantText: "entries must be one of skills/**, config-templates/**",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := Default()
			tc.mutate(cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("Validate() = nil, want error containing %q", tc.wantText)
			}
			if !strings.Contains(err.Error(), tc.wantText) {
				t.Fatalf("Validate() = %q, want substring %q", err.Error(), tc.wantText)
			}
		})
	}
}

func TestConfigValidateValidRemotes(t *testing.T) {
	t.Parallel()
	for _, remote := range []string{
		"https://git.example.com/owner/repo.git",
		"ssh://git@git.example.com/owner/repo.git",
	} {
		t.Run(remote, func(t *testing.T) {
			t.Parallel()
			cfg := Default()
			cfg.Enabled = true
			cfg.Remote = remote
			if err := cfg.Validate(); err != nil {
				t.Fatalf("Validate(%q) = %v, want nil", remote, err)
			}
		})
	}
}
