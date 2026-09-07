package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadMCPDefaultsApplied(t *testing.T) {
	yamlContent := `mcp:
  servers:
    - name: local-files
      transport: stdio
      command: /usr/local/bin/files-mcp
`
	result := writeConfigYAML(t, yamlContent)
	if result.Config.MCP == nil || len(result.Config.MCP.Servers) != 1 {
		t.Fatalf("expected 1 mcp server, got %+v", result.Config.MCP)
	}
	server := result.Config.MCP.Servers[0]
	if server.Name != "local-files" {
		t.Errorf("expected name local-files, got %s", server.Name)
	}
	if time.Duration(server.StartupTimeout) != 10*time.Second {
		t.Errorf("expected StartupTimeout 10s, got %v", server.StartupTimeout)
	}
	if time.Duration(server.RequestTimeout) != 30*time.Second {
		t.Errorf("expected RequestTimeout 30s, got %v", server.RequestTimeout)
	}
	if server.MaxMessageSize != 1024*1024 {
		t.Errorf("expected MaxMessageSize 1MiB, got %v", server.MaxMessageSize)
	}
}

func TestLoadMCPAcceptsValidStdio(t *testing.T) {
	yamlContent := `mcp:
  servers:
    - name: local-files
      transport: stdio
      command: /usr/local/bin/files-mcp
      args: ["--root", "/srv/aura/workspace"]
      environment:
        FOO: bar
      capabilities: ["fs.read"]
      startup_timeout: 5s
      request_timeout: 15s
      max_message_size: 2MiB
      restart:
        max_attempts: 3
        window: 5m
`
	result := writeConfigYAML(t, yamlContent)
	if result.Config.MCP == nil || len(result.Config.MCP.Servers) != 1 {
		t.Fatalf("expected 1 mcp server, got %+v", result.Config.MCP)
	}
	server := result.Config.MCP.Servers[0]
	if server.Name != "local-files" {
		t.Errorf("unexpected name: %s", server.Name)
	}
	if len(server.Args) != 2 || server.Args[0] != "--root" {
		t.Errorf("unexpected args: %v", server.Args)
	}
	if server.Environment["FOO"] != "bar" {
		t.Errorf("unexpected env: %v", server.Environment)
	}
	if len(server.Capabilities) != 1 || server.Capabilities[0] != "fs.read" {
		t.Errorf("unexpected capabilities: %v", server.Capabilities)
	}
	if server.Restart == nil || server.Restart.MaxAttempts != 3 {
		t.Errorf("unexpected restart: %+v", server.Restart)
	}
}

func TestLoadMCPAcceptsValidStreamableHTTP(t *testing.T) {
	yamlContent := `mcp:
  servers:
    - name: remote-example
      transport: streamable_http
      url: https://mcp.example.com/mcp
      auth:
        oauth:
          client_id_env: AURA_MCP_CLIENT_ID
          client_secret_env: AURA_MCP_CLIENT_SECRET
          token_store: keyring
      capabilities: ["external.read"]
      connect_timeout: 5s
      request_timeout: 20s
`
	result := writeConfigYAML(t, yamlContent)
	if result.Config.MCP == nil || len(result.Config.MCP.Servers) != 1 {
		t.Fatalf("expected 1 mcp server, got %+v", result.Config.MCP)
	}
	server := result.Config.MCP.Servers[0]
	if server.Transport != "streamable_http" {
		t.Errorf("unexpected transport: %s", server.Transport)
	}
	if server.URL != "https://mcp.example.com/mcp" {
		t.Errorf("unexpected url: %s", server.URL)
	}
	if server.Auth == nil || server.Auth.OAuth == nil || server.Auth.OAuth.ClientIDEnv != "AURA_MCP_CLIENT_ID" {
		t.Errorf("unexpected auth: %+v", server.Auth)
	}
}

func TestLoadMCPValidationErrors(t *testing.T) {
	cases := []struct {
		name string
		yaml string
	}{
		{
			name: "invalid_server_name",
			yaml: "version: 1\nmcp:\n  servers:\n    - name: INVALID_NAME\n      transport: stdio\n      command: /bin/ls\n",
		},
		{
			name: "duplicate_server_name",
			yaml: "version: 1\nmcp:\n  servers:\n    - name: srv\n      transport: stdio\n      command: /bin/ls\n    - name: srv\n      transport: stdio\n      command: /bin/echo\n",
		},
		{
			name: "unknown_transport",
			yaml: "version: 1\nmcp:\n  servers:\n    - name: srv\n      transport: websocket\n      command: /bin/ls\n",
		},
		{
			name: "stdio_relative_command",
			yaml: "version: 1\nmcp:\n  servers:\n    - name: srv\n      transport: stdio\n      command: bin/ls\n",
		},
		{
			name: "stdio_with_url",
			yaml: "version: 1\nmcp:\n  servers:\n    - name: srv\n      transport: stdio\n      command: /bin/ls\n      url: https://example.com\n",
		},
		{
			name: "stdio_with_auth",
			yaml: "version: 1\nmcp:\n  servers:\n    - name: srv\n      transport: stdio\n      command: /bin/ls\n      auth:\n        static:\n          credential_ref: secret://key\n",
		},
		{
			name: "streamable_http_missing_url",
			yaml: "version: 1\nmcp:\n  servers:\n    - name: srv\n      transport: streamable_http\n",
		},
		{
			name: "streamable_http_with_command",
			yaml: "version: 1\nmcp:\n  servers:\n    - name: srv\n      transport: streamable_http\n      url: https://example.com/mcp\n      command: /bin/ls\n",
		},
		{
			name: "streamable_http_plain_http_non_loopback",
			yaml: "version: 1\nmcp:\n  servers:\n    - name: srv\n      transport: streamable_http\n      url: http://example.com/mcp\n",
		},
		{
			name: "legacy_sse_without_ack",
			yaml: "version: 1\nmcp:\n  servers:\n    - name: srv\n      transport: legacy_sse\n      url: https://example.com/sse\n",
		},
		{
			name: "auth_both_oauth_and_static",
			yaml: "version: 1\nmcp:\n  servers:\n    - name: srv\n      transport: streamable_http\n      url: https://example.com/mcp\n      auth:\n        oauth:\n          client_id_env: ID\n          client_secret_env: SEC\n        static:\n          credential_ref: key\n",
		},
		{
			name: "unknown_key_inside_server",
			yaml: "version: 1\nmcp:\n  servers:\n    - name: srv\n      transport: stdio\n      command: /bin/ls\n      bogus_field: value\n",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.yaml")
			if err := os.WriteFile(path, []byte(tc.yaml), 0o600); err != nil {
				t.Fatalf("write file: %v", err)
			}
			_, err := Load(path)
			if err == nil {
				t.Fatalf("expected config error for case %s, got nil", tc.name)
			}
		})
	}
}
