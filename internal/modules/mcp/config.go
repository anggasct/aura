package mcp

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"time"

	"github.com/anggasct/aura/internal/platform/configkit"
	"github.com/anggasct/aura/internal/platform/egress"
	"github.com/anggasct/aura/internal/platform/secret"
)

const (
	TransportStdio          = "stdio"
	TransportStreamableHTTP = "streamable_http"
	TransportLegacySSE      = "legacy_sse"

	DefaultStartupTimeout = configkit.Duration(10 * time.Second)
	DefaultRequestTimeout = configkit.Duration(30 * time.Second)
	DefaultConnectTimeout = configkit.Duration(10 * time.Second)
	DefaultMaxMessageSize = configkit.ByteSize(1024 * 1024)
)

var serverNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,62}$`)

type Config struct {
	Servers []ServerConfig `json:"servers"`
}

type ServerConfig struct {
	Name             string              `json:"name"`
	Transport        string              `json:"transport"`
	Command          string              `json:"command,omitempty"`
	Args             []string            `json:"args,omitempty"`
	Environment      map[string]string   `json:"environment,omitempty"`
	Capabilities     []string            `json:"capabilities,omitempty"`
	StartupTimeout   configkit.Duration  `json:"startup_timeout,omitempty"`
	RequestTimeout   configkit.Duration  `json:"request_timeout,omitempty"`
	MaxMessageSize   configkit.ByteSize  `json:"max_message_size,omitempty"`
	Restart          *RestartConfig      `json:"restart,omitempty"`
	URL              string              `json:"url,omitempty"`
	Auth             *AuthConfig         `json:"auth,omitempty"`
	ConnectTimeout   *configkit.Duration `json:"connect_timeout,omitempty"`
	AllowRedirects   *bool               `json:"allow_redirects,omitempty"`
	CompatibilityAck bool                `json:"compatibility_ack,omitempty"`
}

type RestartConfig struct {
	MaxAttempts int                `json:"max_attempts"`
	Window      configkit.Duration `json:"window"`
}

type AuthConfig struct {
	OAuth  *OAuthConfig      `json:"oauth,omitempty"`
	Static *StaticAuthConfig `json:"static,omitempty"`
}

type OAuthConfig struct {
	ClientIDRef     secret.Ref `json:"client_id_ref,omitzero"`
	ClientSecretRef secret.Ref `json:"client_secret_ref,omitzero"`
	TokenStore      string     `json:"token_store"`
}

type StaticAuthConfig struct {
	Header        string     `json:"header,omitempty"`
	CredentialRef secret.Ref `json:"credential_ref,omitzero"`
}

func Default() Config {
	return Config{}
}

func (*ServerConfig) Default() any {
	return ServerConfig{
		StartupTimeout: DefaultStartupTimeout,
		RequestTimeout: DefaultRequestTimeout,
		MaxMessageSize: DefaultMaxMessageSize,
	}
}

func (s *ServerConfig) EffectiveConnectTimeout() configkit.Duration {
	if s == nil {
		return 0
	}
	if s.ConnectTimeout != nil {
		return *s.ConnectTimeout
	}
	if s.Transport == TransportStreamableHTTP {
		return DefaultConnectTimeout
	}
	return 0
}

func (c *Config) ServerNames() []string {
	if c == nil || len(c.Servers) == 0 {
		return nil
	}
	names := make([]string, 0, len(c.Servers))
	for i := range c.Servers {
		names = append(names, c.Servers[i].Name)
	}
	return names
}

func (c *Config) FindServer(name string) (*ServerConfig, bool) {
	if c == nil {
		return nil, false
	}
	for i := range c.Servers {
		if c.Servers[i].Name == name {
			return &c.Servers[i], true
		}
	}
	return nil, false
}

func (c *Config) Validate() error {
	if c == nil {
		return nil
	}
	var problems []error
	seen := make(map[string]struct{}, len(c.Servers))
	for i := range c.Servers {
		s := &c.Servers[i]
		nameValid := serverNamePattern.MatchString(s.Name)
		if !nameValid {
			problems = append(problems, configkit.Invalid(fmt.Sprintf("servers[%d].name", i), "must match [a-z][a-z0-9_-]{0,62}"))
		} else {
			if _, exists := seen[s.Name]; exists {
				problems = append(problems, configkit.Invalid(fmt.Sprintf("servers[%d].name", i), fmt.Sprintf("%q is configured more than once", s.Name)))
			}
			seen[s.Name] = struct{}{}
		}
		field := fmt.Sprintf("servers[%s]", s.Name)
		if !nameValid {
			field = fmt.Sprintf("servers[%d]", i)
		}
		if err := ValidateServer(s, field); err != nil {
			problems = append(problems, err)
		}
	}
	return errors.Join(problems...)
}

func ValidateServer(s *ServerConfig, field string) error {
	if s == nil {
		return configkit.Invalid(field, "must not be nil")
	}
	var problems []error
	switch s.Transport {
	case TransportStdio:
		if s.Command == "" {
			problems = append(problems, configkit.Invalid(field+".command", "is required for stdio transport"))
		} else if !filepath.IsAbs(s.Command) {
			problems = append(problems, configkit.Invalid(field+".command", "must be an absolute path"))
		}
		if s.URL != "" {
			problems = append(problems, configkit.Invalid(field+".url", "is not allowed for stdio transport"))
		}
		if s.Auth != nil {
			problems = append(problems, configkit.Invalid(field+".auth", "is not allowed for stdio transport"))
		}
		if s.ConnectTimeout != nil {
			problems = append(problems, configkit.Invalid(field+".connect_timeout", "is not allowed for stdio transport"))
		}
	case TransportStreamableHTTP:
		if s.URL == "" {
			problems = append(problems, configkit.Invalid(field+".url", "is required for streamable_http transport"))
		} else if err := egress.ValidateBaseURL(s.URL); err != nil {
			problems = append(problems, configkit.Invalid(field+".url", "is invalid: "+err.Error()))
		}
		if s.Command != "" || len(s.Args) > 0 {
			problems = append(problems, configkit.Invalid(field+".command", "and args are not allowed for streamable_http transport"))
		}
		if s.ConnectTimeout != nil && *s.ConnectTimeout <= 0 {
			problems = append(problems, configkit.Invalid(field+".connect_timeout", "must be positive"))
		}
	case TransportLegacySSE:
		if !s.CompatibilityAck {
			problems = append(problems, configkit.Invalid(field, "legacy_sse requires compatibility_ack: true"))
		}
		if s.URL == "" {
			problems = append(problems, configkit.Invalid(field+".url", "is required for legacy_sse transport"))
		} else if err := egress.ValidateBaseURL(s.URL); err != nil {
			problems = append(problems, configkit.Invalid(field+".url", "is invalid: "+err.Error()))
		}
		if s.ConnectTimeout != nil && *s.ConnectTimeout <= 0 {
			problems = append(problems, configkit.Invalid(field+".connect_timeout", "must be positive"))
		}
	default:
		problems = append(problems, configkit.Invalid(field+".transport", "must be stdio, streamable_http, or legacy_sse"))
	}

	if s.StartupTimeout <= 0 {
		problems = append(problems, configkit.Invalid(field+".startup_timeout", "must be positive"))
	}
	if s.RequestTimeout <= 0 {
		problems = append(problems, configkit.Invalid(field+".request_timeout", "must be positive"))
	}
	if s.MaxMessageSize <= 0 {
		problems = append(problems, configkit.Invalid(field+".max_message_size", "must be positive"))
	}

	if s.Restart != nil {
		if s.Restart.MaxAttempts < 0 {
			problems = append(problems, configkit.Invalid(field+".restart.max_attempts", "must not be negative"))
		}
		if s.Restart.Window < 0 {
			problems = append(problems, configkit.Invalid(field+".restart.window", "must not be negative"))
		}
	}

	if s.Auth != nil && s.Transport != TransportStdio {
		if s.Auth.OAuth != nil && s.Auth.Static != nil {
			problems = append(problems, configkit.Invalid(field+".auth", "must declare either oauth or static, not both"))
		}
		if s.Auth.OAuth == nil && s.Auth.Static == nil {
			problems = append(problems, configkit.Invalid(field+".auth", "must declare either oauth or static"))
		}
		if s.Auth.OAuth != nil {
			if s.Auth.OAuth.ClientIDRef.IsZero() {
				problems = append(problems, configkit.Invalid(field+".auth.oauth.client_id_ref", "is required"))
			}
			if s.Auth.OAuth.ClientSecretRef.IsZero() {
				problems = append(problems, configkit.Invalid(field+".auth.oauth.client_secret_ref", "is required"))
			}
		}
		if s.Auth.Static != nil {
			if s.Auth.Static.CredentialRef.IsZero() {
				problems = append(problems, configkit.Invalid(field+".auth.static.credential_ref", "is required"))
			}
		}
	}
	return errors.Join(problems...)
}

var _ configkit.Defaulter = (*ServerConfig)(nil)
