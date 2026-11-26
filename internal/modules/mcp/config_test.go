package mcp

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/configkit"
	"github.com/anggasct/aura/internal/platform/secret"
)

func mustSecretRef(t *testing.T, raw string) secret.Ref {
	t.Helper()
	ref, err := secret.ParseRef(raw)
	if err != nil {
		t.Fatalf("ParseRef(%q): %v", raw, err)
	}
	return ref
}

func durationPtr(d time.Duration) *configkit.Duration {
	v := configkit.Duration(d)
	return &v
}

func TestValidateMCPServerNamesTheField(t *testing.T) {
	err := ValidateServer(&ServerConfig{
		Transport:      TransportStdio,
		Command:        "relative",
		StartupTimeout: DefaultStartupTimeout,
		RequestTimeout: DefaultRequestTimeout,
		MaxMessageSize: DefaultMaxMessageSize,
	}, "plugin.backend")
	if err == nil || !strings.Contains(err.Error(), "plugin.backend.command") {
		t.Fatalf("ValidateServer error = %v", err)
	}
}

func TestMCPServerRejectsExplicitZeroLimits(t *testing.T) {
	base := func() ServerConfig {
		return ServerConfig{
			Name:           "docs",
			Transport:      TransportStdio,
			Command:        "/usr/local/bin/docs-mcp",
			StartupTimeout: DefaultStartupTimeout,
			RequestTimeout: DefaultRequestTimeout,
			MaxMessageSize: DefaultMaxMessageSize,
		}
	}

	cases := []struct {
		name      string
		mutate    func(*ServerConfig)
		wantField string
	}{
		{
			name:      "zero startup_timeout",
			mutate:    func(s *ServerConfig) { s.StartupTimeout = 0 },
			wantField: "servers[docs].startup_timeout",
		},
		{
			name:      "zero request_timeout",
			mutate:    func(s *ServerConfig) { s.RequestTimeout = 0 },
			wantField: "servers[docs].request_timeout",
		},
		{
			name:      "zero max_message_size",
			mutate:    func(s *ServerConfig) { s.MaxMessageSize = 0 },
			wantField: "servers[docs].max_message_size",
		},
		{
			name: "zero connect_timeout on streamable_http",
			mutate: func(s *ServerConfig) {
				s.Transport = TransportStreamableHTTP
				s.Command = ""
				s.URL = "https://mcp.example.com/mcp"
				s.ConnectTimeout = durationPtr(0)
			},
			wantField: "servers[docs].connect_timeout",
		},
		{
			name: "zero connect_timeout on legacy_sse",
			mutate: func(s *ServerConfig) {
				s.Transport = TransportLegacySSE
				s.Command = ""
				s.URL = "https://mcp.example.com/sse"
				s.CompatibilityAck = true
				s.ConnectTimeout = durationPtr(0)
			},
			wantField: "servers[docs].connect_timeout",
		},
		{
			name: "connect_timeout set on stdio",
			mutate: func(s *ServerConfig) {
				s.ConnectTimeout = durationPtr(5 * time.Second)
			},
			wantField: "servers[docs].connect_timeout",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := base()
			tc.mutate(&srv)
			cfg := &Config{Servers: []ServerConfig{srv}}
			err := cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.wantField) {
				t.Fatalf("Validate() = %v, want field %q", err, tc.wantField)
			}
		})
	}
}

func TestMCPConfigValidateRejectsEmptyStaticCredentialRef(t *testing.T) {
	cfg := &Config{
		Servers: []ServerConfig{
			{
				Name:           "remote",
				Transport:      TransportStreamableHTTP,
				URL:            "https://mcp.example.com/mcp",
				StartupTimeout: DefaultStartupTimeout,
				RequestTimeout: DefaultRequestTimeout,
				MaxMessageSize: DefaultMaxMessageSize,
				Auth: &AuthConfig{
					Static: &StaticAuthConfig{Header: "Authorization"},
				},
			},
		},
	}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "servers[remote].auth.static.credential_ref") {
		t.Fatalf("Validate() = %v, want static.credential_ref error", err)
	}
}

func TestMCPConfigValidateOAuthAndLegacySSE(t *testing.T) {
	t.Run("legacy_sse allows omitted or positive connect_timeout", func(t *testing.T) {
		for _, ct := range []*configkit.Duration{nil, durationPtr(5 * time.Second)} {
			cfg := &Config{
				Servers: []ServerConfig{
					{
						Name:             "legacy",
						Transport:        TransportLegacySSE,
						URL:              "https://legacy.example.com/sse",
						CompatibilityAck: true,
						StartupTimeout:   DefaultStartupTimeout,
						RequestTimeout:   DefaultRequestTimeout,
						MaxMessageSize:   DefaultMaxMessageSize,
						ConnectTimeout:   ct,
					},
				},
			}
			if err := cfg.Validate(); err != nil {
				t.Fatalf("Validate(ct=%v) = %v, want nil", ct, err)
			}
		}
	})

	t.Run("oauth requires both refs", func(t *testing.T) {
		cfg := &Config{
			Servers: []ServerConfig{
				{
					Name:           "remote",
					Transport:      TransportStreamableHTTP,
					URL:            "https://mcp.example.com/mcp",
					StartupTimeout: DefaultStartupTimeout,
					RequestTimeout: DefaultRequestTimeout,
					MaxMessageSize: DefaultMaxMessageSize,
					Auth:           &AuthConfig{OAuth: &OAuthConfig{}},
				},
			},
		}
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), "client_id_ref") || !strings.Contains(err.Error(), "client_secret_ref") {
			t.Fatalf("Validate() = %v, want both client_id_ref and client_secret_ref", err)
		}

		cfg.Servers[0].Auth.OAuth.ClientIDRef = mustSecretRef(t, "env://MCP_CLIENT_ID")
		cfg.Servers[0].Auth.OAuth.ClientSecretRef = mustSecretRef(t, "file:///run/secrets/mcp-secret")
		if err := cfg.Validate(); err != nil {
			t.Fatalf("Validate() with valid oauth refs = %v", err)
		}
	})
}

func TestMCPConfigHelpersAndDefaults(t *testing.T) {
	def, ok := (&ServerConfig{}).Default().(ServerConfig)
	if !ok {
		t.Fatalf("Default() did not return ServerConfig")
	}
	if def.StartupTimeout != DefaultStartupTimeout || def.RequestTimeout != DefaultRequestTimeout || def.MaxMessageSize != DefaultMaxMessageSize || def.ConnectTimeout != nil {
		t.Fatalf("Default() = %+v", def)
	}

	stdio := &ServerConfig{Transport: TransportStdio}
	if got := stdio.EffectiveConnectTimeout(); got != 0 {
		t.Fatalf("stdio EffectiveConnectTimeout() = %v, want 0", got)
	}
	legacy := &ServerConfig{Transport: TransportLegacySSE}
	if got := legacy.EffectiveConnectTimeout(); got != 0 {
		t.Fatalf("legacy_sse EffectiveConnectTimeout() = %v, want 0", got)
	}
	streamable := &ServerConfig{Transport: TransportStreamableHTTP}
	if got := streamable.EffectiveConnectTimeout(); got != DefaultConnectTimeout {
		t.Fatalf("streamable_http EffectiveConnectTimeout() = %v, want %v", got, DefaultConnectTimeout)
	}
	custom := durationPtr(25 * time.Second)
	streamable.ConnectTimeout = custom
	if got := streamable.EffectiveConnectTimeout(); got != *custom {
		t.Fatalf("custom EffectiveConnectTimeout() = %v, want %v", got, *custom)
	}

	cfg := &Config{
		Servers: []ServerConfig{
			{Name: "alpha"},
			{Name: "beta"},
		},
	}
	if got := cfg.ServerNames(); !slices.Equal(got, []string{"alpha", "beta"}) {
		t.Fatalf("ServerNames() = %v", got)
	}
	if srv, found := cfg.FindServer("beta"); !found || srv.Name != "beta" {
		t.Fatalf("FindServer(beta) = %v, %v", srv, found)
	}
	if _, found := cfg.FindServer("missing"); found {
		t.Fatal("FindServer(missing) = true")
	}
	var nilCfg *Config
	if nilCfg.ServerNames() != nil {
		t.Fatal("nil ServerNames() != nil")
	}
	if _, found := nilCfg.FindServer("a"); found {
		t.Fatal("nil FindServer() = true")
	}
}
