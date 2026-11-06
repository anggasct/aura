package plugin

import (
	"strings"
	"testing"
	"time"
)

func TestParseManifestAcceptsTheBaseManifest(t *testing.T) {
	manifest, err := ParseManifest([]byte(baseManifest))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	if manifest.ID != "weather" || len(manifest.Tools) != 2 || manifest.Backend.Command != "./server" {
		t.Fatalf("manifest = %+v", manifest)
	}
}

func TestParseManifestAcceptsStreamableHTTPWithStaticSecret(t *testing.T) {
	text := `api_version: aura.plugin/v1
id: remote-notes
version: 0.3.1
name: Notes
description: Remote notes.
tools:
  - name: search
    description: Search notes.
    tier: read
egress: [notes.example.com]
secrets:
  - name: token
    ref: env://NOTES_TOKEN
auth:
  kind: static_secret
  secret: token
  header: Authorization
backend:
  kind: mcp
  transport: streamable_http
  url: https://notes.example.com/mcp
  request_timeout: 15s
  max_message_size: 512KiB
`
	manifest, err := ParseManifest([]byte(text))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	server := manifest.ServerConfig("/data/plugins/remote-notes/0.3.1")
	if server.Auth == nil || server.Auth.Static == nil || server.Auth.Static.CredentialRef != "env://NOTES_TOKEN" || server.URL != "https://notes.example.com/mcp" {
		t.Fatalf("server = %+v", server)
	}
	if server.MaxMessageSize != 512<<10 || time.Duration(server.RequestTimeout) != 15*time.Second {
		t.Fatalf("server limits = %+v", server)
	}
}

func TestParseManifestRejects(t *testing.T) {
	cases := []struct {
		name string
		text func(t *testing.T) string
		want ErrorCode
	}{
		{"not yaml", func(*testing.T) string { return "a: [" }, ErrManifestInvalid},
		{"unknown top level key", func(t *testing.T) string { return baseManifest + "extra: true\n" }, ErrManifestInvalid},
		{"unknown backend key", func(t *testing.T) string { return baseManifest + "  environment: {A: b}\n" }, ErrManifestInvalid},
		{"duplicate key", func(t *testing.T) string { return baseManifest + "name: Again\n" }, ErrManifestInvalid},
		{"two documents", func(t *testing.T) string { return baseManifest + "---\n" + baseManifest }, ErrManifestInvalid},
		{"wrong api version", func(t *testing.T) string { return replaceOnce(t, baseManifest, "aura.plugin/v1", "aura.plugin/v2") }, ErrManifestInvalid},
		{"uppercase id", func(t *testing.T) string { return replaceOnce(t, baseManifest, "id: weather", "id: Weather") }, ErrManifestInvalid},
		{"traversal id", func(t *testing.T) string { return replaceOnce(t, baseManifest, "id: weather", "id: ../weather") }, ErrManifestInvalid},
		{"slash id", func(t *testing.T) string { return replaceOnce(t, baseManifest, "id: weather", "id: a/b") }, ErrManifestInvalid},
		{"underscore id", func(t *testing.T) string { return replaceOnce(t, baseManifest, "id: weather", "id: we_ather") }, ErrManifestInvalid},
		{"traversal version", func(t *testing.T) string { return replaceOnce(t, baseManifest, "version: 1.0.0", "version: ../1.0.0") }, ErrManifestInvalid},
		{"short version", func(t *testing.T) string { return replaceOnce(t, baseManifest, "version: 1.0.0", `version: "1.0"`) }, ErrManifestInvalid},
		{"empty name", func(t *testing.T) string { return replaceOnce(t, baseManifest, "name: Weather", `name: ""`) }, ErrManifestInvalid},
		{"control characters in description", func(t *testing.T) string {
			return replaceOnce(t, baseManifest, "description: Weather forecasts.", `description: "bad \x07 text"`)
		}, ErrManifestInvalid},
		{"no tools", func(t *testing.T) string {
			return replaceOnce(t, baseManifest, "tools:\n  - name: forecast\n    description: Get a forecast for a city.\n    tier: read\n  - name: alerts\n    description: List active alerts.\n    tier: read\n", "tools: []\n")
		}, ErrManifestInvalid},
		{"duplicate tool", func(t *testing.T) string { return replaceOnce(t, baseManifest, "name: alerts", "name: forecast") }, ErrManifestInvalid},
		{"tool name with a dot", func(t *testing.T) string { return replaceOnce(t, baseManifest, "name: alerts", "name: alerts.v2") }, ErrManifestInvalid},
		{"tool wire name too long", func(t *testing.T) string {
			return replaceOnce(t, baseManifest, "name: alerts", "name: "+strings.Repeat("a", 60))
		}, ErrManifestInvalid},
		{"unknown tier", func(t *testing.T) string {
			return replaceOnce(t, baseManifest, "tier: read\n  - name: alerts", "tier: admin\n  - name: alerts")
		}, ErrManifestInvalid},
		{"capability uppercase", func(t *testing.T) string { return replaceOnce(t, baseManifest, "[network]", "[Network]") }, ErrManifestInvalid},
		{"egress wildcard", func(t *testing.T) string {
			return replaceOnce(t, baseManifest, "[api.weather.example]", `["*.weather.example"]`)
		}, ErrManifestInvalid},
		{"egress port out of range", func(t *testing.T) string {
			return replaceOnce(t, baseManifest, "[api.weather.example]", "[api.weather.example:70000]")
		}, ErrManifestInvalid},
		{"egress with scheme", func(t *testing.T) string {
			return replaceOnce(t, baseManifest, "[api.weather.example]", "[https://api.weather.example]")
		}, ErrManifestInvalid},
		{"auth oauth2", func(t *testing.T) string { return replaceOnce(t, baseManifest, "kind: none", "kind: oauth2") }, ErrManifestInvalid},
		{"auth none with a secret", func(t *testing.T) string {
			return replaceOnce(t, baseManifest, "secrets: []", "secrets:\n  - name: token\n    ref: env://TOKEN")
		}, ErrManifestInvalid},
		{"static secret on stdio", func(t *testing.T) string {
			text := replaceOnce(t, baseManifest, "secrets: []", "secrets:\n  - name: token\n    ref: env://TOKEN")
			return replaceOnce(t, text, "kind: none", "kind: static_secret\n  secret: token")
		}, ErrManifestInvalid},
		{"backend connector", func(t *testing.T) string { return replaceOnce(t, baseManifest, "kind: mcp", "kind: connector") }, ErrBackendUnsupported},
		{"backend script", func(t *testing.T) string { return replaceOnce(t, baseManifest, "kind: mcp", "kind: script") }, ErrBackendUnsupported},
		{"backend browser", func(t *testing.T) string { return replaceOnce(t, baseManifest, "kind: mcp", "kind: browser") }, ErrBackendUnsupported},
		{"backend unknown kind", func(t *testing.T) string { return replaceOnce(t, baseManifest, "kind: mcp", "kind: wasm") }, ErrBackendUnsupported},
		{"backend connector with its own fields", func(t *testing.T) string {
			return replaceOnce(t, baseManifest, "  kind: mcp\n  transport: stdio\n  command: ./server\n  args: [--quiet]\n", "  kind: connector\n  rpc: v1\n")
		}, ErrBackendUnsupported},
		{"backend missing", func(t *testing.T) string { return strings.Split(baseManifest, "backend:")[0] }, ErrManifestInvalid},
		{"absolute command", func(t *testing.T) string {
			return replaceOnce(t, baseManifest, "command: ./server", "command: /bin/sh")
		}, ErrManifestInvalid},
		{"traversal command", func(t *testing.T) string {
			return replaceOnce(t, baseManifest, "command: ./server", "command: ../server")
		}, ErrManifestInvalid},
		{"nested traversal command", func(t *testing.T) string {
			return replaceOnce(t, baseManifest, "command: ./server", "command: bin/../../server")
		}, ErrManifestInvalid},
		{"empty command", func(t *testing.T) string { return replaceOnce(t, baseManifest, "command: ./server", `command: ""`) }, ErrManifestInvalid},
		{"legacy sse transport", func(t *testing.T) string {
			return replaceOnce(t, baseManifest, "transport: stdio", "transport: legacy_sse")
		}, ErrManifestInvalid},
		{"stdio with url", func(t *testing.T) string {
			return replaceOnce(t, baseManifest, "  args: [--quiet]", "  url: https://x.example/mcp")
		}, ErrManifestInvalid},
		{"negative timeout", func(t *testing.T) string {
			return replaceOnce(t, baseManifest, "  args: [--quiet]", "  request_timeout: -5s")
		}, ErrManifestInvalid},
		{"http url host not in egress", func(t *testing.T) string {
			return replaceOnce(t, baseManifest, "  transport: stdio\n  command: ./server\n  args: [--quiet]", "  transport: streamable_http\n  url: https://other.example/mcp")
		}, ErrManifestInvalid},
		{"http url with credentials", func(t *testing.T) string {
			return replaceOnce(t, baseManifest, "  transport: stdio\n  command: ./server\n  args: [--quiet]", "  transport: streamable_http\n  url: https://user:pw@api.weather.example/mcp")
		}, ErrManifestInvalid},
		{"http url with plain http", func(t *testing.T) string {
			return replaceOnce(t, baseManifest, "  transport: stdio\n  command: ./server\n  args: [--quiet]", "  transport: streamable_http\n  url: http://api.weather.example/mcp")
		}, ErrManifestInvalid},
		{"oversize manifest", func(t *testing.T) string { return baseManifest + "# " + strings.Repeat("x", MaxManifestBytes) + "\n" }, ErrManifestTooLarge},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseManifest([]byte(tc.text(t)))
			if !hasCode(err, tc.want) {
				t.Fatalf("ParseManifest error = %v, want %s", err, tc.want)
			}
		})
	}
}

func TestParseManifestErrorsNeverEchoSecretLookingValues(t *testing.T) {
	const leaked = "sk-live-0123456789abcdef"
	cases := map[string]string{
		"secret reference holding a value": replaceOnce(t, strings.Replace(baseManifest, "secrets: []", "secrets:\n  - name: token\n    ref: "+leaked, 1), "kind: none", "kind: static_secret\n  secret: token"),
		"url credentials":                  replaceOnce(t, baseManifest, "  transport: stdio\n  command: ./server\n  args: [--quiet]", "  transport: streamable_http\n  url: https://user:"+leaked+"@api.weather.example/mcp"),
		"malformed url":                    replaceOnce(t, baseManifest, "  transport: stdio\n  command: ./server\n  args: [--quiet]", "  transport: streamable_http\n  url: \"https://api.weather.example/%zz"+leaked+"\""),
		"unknown key with a value":         baseManifest + leaked + ": " + leaked + "\n",
	}
	for name, text := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ParseManifest([]byte(text))
			if err == nil {
				t.Fatal("ParseManifest accepted the manifest")
			}
			if strings.Contains(err.Error(), leaked) {
				t.Fatalf("error leaks the value: %v", err)
			}
		})
	}
}

func TestParseManifestReportsEveryProblem(t *testing.T) {
	text := replaceOnce(t, baseManifest, "id: weather", "id: Weather")
	text = replaceOnce(t, text, "version: 1.0.0", "version: nope")
	_, err := ParseManifest([]byte(text))
	if !hasCode(err, ErrManifestInvalid) || !strings.Contains(err.Error(), "id must") || !strings.Contains(err.Error(), "version must") {
		t.Fatalf("error = %v", err)
	}
}

func TestServerConfigResolvesTheCommandInsideTheBundle(t *testing.T) {
	manifest, err := ParseManifest([]byte(replaceOnce(t, baseManifest, "command: ./server", "command: bin/server")))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	server := manifest.ServerConfig("/data/plugins/weather/1.0.0")
	if server.Command != "/data/plugins/weather/1.0.0/bin/server" || server.Transport != "stdio" || server.Name != "weather" {
		t.Fatalf("server = %+v", server)
	}
	if len(server.Capabilities) != 1 || server.Capabilities[0] != "network" || server.RequestTimeout == 0 || server.MaxMessageSize == 0 {
		t.Fatalf("server defaults = %+v", server)
	}
}
