package config

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/capability"
)

const testdataDir = "../../testdata"

func fixture(t *testing.T, name string) string {
	t.Helper()
	return filepath.Join(testdataDir, "config", name)
}

func TestMarshalDefault_GoldenFile(t *testing.T) {
	defaults := Default()
	got, err := Marshal(&defaults)
	if err != nil {
		t.Fatalf("Marshal(Default()): %v", err)
	}
	want, err := os.ReadFile(filepath.Join(testdataDir, "golden", "config.yaml"))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("golden mismatch\n--- got ---\n%s--- want ---\n%s", got, want)
	}
}

func TestLoad_ValidFile(t *testing.T) {
	res, err := Load(fixture(t, "valid.yaml"))
	if err != nil {
		t.Fatalf("Load(valid): %v", err)
	}
	cfg := res.Config
	if cfg.Version != 1 {
		t.Errorf("Version = %d, want 1", cfg.Version)
	}
	if cfg.Server.Host != "0.0.0.0" || cfg.Server.Port != 9090 {
		t.Errorf("Server = %+v, want host 0.0.0.0 port 9090", cfg.Server)
	}
	if got := time.Duration(cfg.Runtime.ShutdownTimeout); got != 10*time.Second {
		t.Errorf("ShutdownTimeout = %v, want 10s", got)
	}
	if cfg.Runtime.MaxActiveTurns != 8 || cfg.Runtime.MaxPendingTurns != 128 {
		t.Errorf("Runtime limits = %+v, want 8 active and 128 pending", cfg.Runtime)
	}
	if cfg.Logging.Level != "debug" || cfg.Logging.Format != "json" {
		t.Errorf("Logging = %+v, want debug/json", cfg.Logging)
	}
	if res.DefaultGenerated {
		t.Error("DefaultGenerated = true for an existing file")
	}
}

func TestLoad_EnvOverride(t *testing.T) {
	t.Setenv("AURA_SERVER_PORT", "7777")
	t.Setenv("AURA_RUNTIME_SHUTDOWN_TIMEOUT", "45s")
	t.Setenv("AURA_LOGGING_LEVEL", "warn")

	res, err := Load(fixture(t, "valid.yaml"))
	if err != nil {
		t.Fatalf("Load with env: %v", err)
	}
	cfg := res.Config
	if cfg.Server.Port != 7777 {
		t.Errorf("Port = %d, want 7777 (env override)", cfg.Server.Port)
	}
	if got := time.Duration(cfg.Runtime.ShutdownTimeout); got != 45*time.Second {
		t.Errorf("ShutdownTimeout = %v, want 45s (env override)", got)
	}
	if cfg.Logging.Level != "warn" {
		t.Errorf("Level = %q, want warn (env override)", cfg.Logging.Level)
	}
	if cfg.Logging.Format != "json" {
		t.Errorf("Format = %q, want json (unchanged from file)", cfg.Logging.Format)
	}
}

func TestLoad_EnvOverrideDoesNotPersist(t *testing.T) {
	src := fixture(t, "valid.yaml")
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	dst := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(dst, data, 0o600); err != nil {
		t.Fatalf("write temp config: %v", err)
	}

	t.Setenv("AURA_SERVER_PORT", "5555")
	if _, err := Load(dst); err != nil {
		t.Fatalf("Load: %v", err)
	}
	after, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if !bytes.Equal(after, data) {
		t.Fatalf("config file was modified by env override\nbefore:\n%s\nafter:\n%s", data, after)
	}
}

func TestLoad_MissingDefaultAutoGenerates(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)

	res, err := Load("")
	if err != nil {
		t.Fatalf("Load default: %v", err)
	}
	cfg := res.Config
	if cfg.Server.Port != 8280 {
		t.Errorf("Port = %d, want default 8280", cfg.Server.Port)
	}
	if !res.DefaultGenerated {
		t.Error("DefaultGenerated = false, want true for a missing default config")
	}

	path := filepath.Join(xdg, "aura", "config.yaml")
	if res.Path != path {
		t.Errorf("Path = %q, want %q", res.Path, path)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("expected generated config at %s: %v", path, err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("generated config perm = %o, want 600", perm)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read generated: %v", err)
	}
	defaults := Default()
	want, err := Marshal(&defaults)
	if err != nil {
		t.Fatalf("marshal default: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("generated config != Default()\n--- got ---\n%s--- want ---\n%s", got, want)
	}
}

func TestLoad_MalformedYAML(t *testing.T) {
	_, err := Load(fixture(t, "invalid.yaml"))
	if err == nil {
		t.Fatal("expected error for malformed YAML, got nil")
	}
	if !strings.HasPrefix(err.Error(), "config: ") {
		t.Errorf("error %q missing config component prefix", err)
	}
	if !strings.Contains(err.Error(), "invalid YAML") {
		t.Errorf("error %q does not mention invalid YAML", err)
	}
	if !strings.Contains(err.Error(), "line") {
		t.Errorf("error %q does not specify a line", err)
	}
}

func TestLoad_UnknownKey(t *testing.T) {
	_, err := Load(fixture(t, "unknown_keys.yaml"))
	if err == nil {
		t.Fatal("expected error for unknown key, got nil")
	}
	if !strings.HasPrefix(err.Error(), "config: ") {
		t.Errorf("error %q missing config component prefix", err)
	}
	if !strings.Contains(err.Error(), `unknown key "server.typo"`) {
		t.Errorf("error %q does not name the offending key", err)
	}
	if !strings.Contains(err.Error(), "line") {
		t.Errorf("error %q does not specify a line", err)
	}
}

func TestLoad_ExplicitPathMissing(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "does-not-exist.yaml"))
	if err == nil {
		t.Fatal("expected error for missing explicit path, got nil")
	}
	if !strings.Contains(err.Error(), "config: file not found") {
		t.Errorf("error %q does not report file not found under config prefix", err)
	}
}

func TestLoad_WrongVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("version: 2\nserver:\n  host: 127.0.0.1\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for unsupported version, got nil")
	}
	if code, ok := CodeOf(err); !ok || code != ErrorCodeVersionUnsupported {
		t.Errorf("CodeOf(%v) = %q, %v; want %q, true", err, code, ok, ErrorCodeVersionUnsupported)
	}
	if !strings.Contains(err.Error(), `unsupported version "2" (supported: 1)`) {
		t.Errorf("error %q does not report unsupported version", err)
	}
}

func TestLoad_EnvironmentCannotUpgradeSchemaVersion(t *testing.T) {
	path := writeTempConfig(t, "version: 1\nserver:\n  host: 127.0.0.1\n")
	t.Setenv("AURA_VERSION", "2")
	_, err := Load(path)
	if code, ok := CodeOf(err); !ok || code != ErrorCodeVersionUnsupported {
		t.Fatalf("CodeOf(%v) = %q, %v; want %q, true", err, code, ok, ErrorCodeVersionUnsupported)
	}
}

func TestLoad_StrictRuntimeAndCapabilityShapes(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{
			name:    "capability scalar",
			content: "version: 1\ncapabilities:\n  enabled: sample\n",
			want:    "capabilities.enabled must be a sequence",
		},
		{
			name:    "runtime quoted integer",
			content: "version: 1\nruntime:\n  max_active_turns: \"4\"\n",
			want:    "runtime.max_active_turns must be an integer",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(writeTempConfig(t, tt.content))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Load error = %v, want substring %q", err, tt.want)
			}
		})
	}
}

func TestLoad_ExplicitZeroRuntimeValuesRejected(t *testing.T) {
	path := writeTempConfig(t, "version: 1\nruntime:\n  max_active_turns: 0\n")
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "runtime.max_active_turns must be positive") {
		t.Fatalf("Load error = %v, want explicit zero to be rejected", err)
	}
}

func TestLoad_PathIsDirectory(t *testing.T) {
	_, err := Load(t.TempDir())
	if err == nil {
		t.Fatal("expected error when config path is a directory, got nil")
	}
	if !strings.Contains(err.Error(), "is a directory") {
		t.Errorf("error %q does not report directory", err)
	}
}

func TestLoad_AuraConfigEnvPath(t *testing.T) {
	t.Setenv("AURA_CONFIG", fixture(t, "valid.yaml"))
	res, err := Load("")
	if err != nil {
		t.Fatalf("Load with AURA_CONFIG: %v", err)
	}
	if res.Config.Server.Port != 9090 {
		t.Errorf("Port = %d, want 9090 from AURA_CONFIG file", res.Config.Server.Port)
	}
}

func TestLoad_AuraConfigEnvMissingErrors(t *testing.T) {
	t.Setenv("AURA_CONFIG", filepath.Join(t.TempDir(), "absent.yaml"))
	_, err := Load("")
	if err == nil {
		t.Fatal("expected error for missing AURA_CONFIG path, got nil")
	}
	if !strings.Contains(err.Error(), "config: file not found") {
		t.Errorf("error %q does not report file not found under config prefix", err)
	}
}

func TestLoad_WithModelsSection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	content := `version: 1
server:
  host: 127.0.0.1
models:
  definitions:
    primary:
      protocol: anthropic_messages
      model: claude-sonnet-4-20250514
      base_url: https://api.anthropic.com
      api_key_env: ANTHROPIC_API_KEY
      capabilities:
        streaming: true
        tools: true
        structured_output: false
        vision: true
        audio: false
        reasoning: true
        context_tokens: 200000
        tokenizer: anthropic
        usage_reporting: true
  routing:
    summarize: primary
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	res, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cfg := res.Config
	primary := cfg.Models.Definitions["primary"]
	if primary.Protocol != "anthropic_messages" || primary.Model != "claude-sonnet-4-20250514" || primary.APIKeyEnv != "ANTHROPIC_API_KEY" {
		t.Errorf("primary = %+v", primary)
	}
	if cfg.Models.Routing["summarize"] != "primary" {
		t.Errorf("routing.summarize = %q, want primary", cfg.Models.Routing["summarize"])
	}
	if cfg.Models.RequestTimeout != 0 {
		t.Errorf("RequestTimeout should default to 0 when unset, got %v", cfg.Models.RequestTimeout)
	}
	if cfg.Server.Host != "127.0.0.1" || cfg.Server.Port != 8280 {
		t.Errorf("Server defaults = %+v", cfg.Server)
	}
	if cfg.Runtime.MaxActiveTurns != 4 || cfg.Runtime.MaxPendingTurns != 64 {
		t.Errorf("Runtime defaults = %+v", cfg.Runtime)
	}
	if cfg.Logging.Level != "info" || cfg.Logging.Format != "text" {
		t.Errorf("Logging defaults = %+v", cfg.Logging)
	}
}

func TestLoad_UnknownKeyInsideModelsRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	content := `version: 1
models:
  definitions:
    primary:
      protocol: anthropic_messages
      model: claude-sonnet-4-20250514
      api_key_env: ANTHROPIC_API_KEY
      capabilities:
        streaming: true
        tools: true
        structured_output: false
        vision: true
        audio: false
        reasoning: true
        context_tokens: 200000
        tokenizer: anthropic
        usage_reporting: true
      typo_key: nope
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected unknown key error inside models")
	}
	if !strings.Contains(err.Error(), `unknown key "models.definitions.primary.typo_key"`) {
		t.Errorf("error %q does not name the offending key", err)
	}
}

func TestLoad_ModelProtocolAndCapabilityValidation(t *testing.T) {
	valid := `version: 1
models:
  definitions:
    primary:
      protocol: anthropic_messages
      model: claude-sonnet
      base_url: https://api.anthropic.com
      api_key_env: ANTHROPIC_API_KEY
      capabilities:
        streaming: true
        tools: true
        structured_output: false
        vision: true
        audio: false
        reasoning: true
        context_tokens: 200000
        tokenizer: anthropic
        usage_reporting: true
`
	tests := []struct {
		name    string
		content string
		code    ErrorCode
		want    string
	}{
		{
			name:    "unknown protocol",
			content: strings.Replace(valid, "anthropic_messages", "unknown_protocol", 1),
			code:    ErrorCodeModelProtocolInvalid,
			want:    "unsupported protocol",
		},
		{
			name:    "missing capabilities",
			content: strings.Replace(valid, "      capabilities:\n        streaming: true\n        tools: true\n        structured_output: false\n        vision: true\n        audio: false\n        reasoning: true\n        context_tokens: 200000\n        tokenizer: anthropic\n        usage_reporting: true\n", "", 1),
			code:    ErrorCodeModelCapabilityUnsupported,
			want:    "requires a capabilities mapping",
		},
		{
			name:    "both secret references",
			content: strings.Replace(valid, "      api_key_env: ANTHROPIC_API_KEY\n", "      api_key_env: ANTHROPIC_API_KEY\n      api_key_file: /run/secrets/anthropic\n", 1),
			code:    ErrorCodeModelSecretInvalid,
			want:    "only one of api_key_env or api_key_file",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(writeTempConfig(t, tt.content))
			if code, ok := CodeOf(err); !ok || code != tt.code {
				t.Fatalf("CodeOf(%v) = %q, %v; want %q, true", err, code, ok, tt.code)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not contain %q", err, tt.want)
			}
		})
	}
}

func TestLoad_InlineModelAPIKeyRejected(t *testing.T) {
	content := `version: 1
models:
  definitions:
    primary:
      protocol: anthropic_messages
      model: claude-sonnet
      base_url: https://api.anthropic.com
      api_key_env: ANTHROPIC_API_KEY
      api_key: inline-secret
      capabilities:
        streaming: true
        tools: true
        structured_output: false
        vision: true
        audio: false
        reasoning: true
        context_tokens: 200000
        tokenizer: anthropic
        usage_reporting: true
`
	_, err := Load(writeTempConfig(t, content))
	if err == nil || !strings.Contains(err.Error(), `unknown key "models.definitions.primary.api_key"`) {
		t.Fatalf("Load error = %v, want inline API key rejection", err)
	}
}

func TestLoad_ModelSecretReferencesAndLoopback(t *testing.T) {
	content := `version: 1
models:
  definitions:
    primary:
      protocol: anthropic_messages
      model: claude-sonnet
      base_url: https://api.anthropic.com
      api_key_env: ANTHROPIC_API_KEY
      capabilities:
        streaming: true
        tools: true
        structured_output: false
        vision: true
        audio: false
        reasoning: true
        context_tokens: 200000
        tokenizer: anthropic
        usage_reporting: true
    auxiliary:
      protocol: openai_chat_compat
      model: local-model
      base_url: http://127.0.0.1:11434/v1
      capabilities:
        streaming: true
        tools: false
        structured_output: false
        vision: false
        audio: false
        reasoning: false
        context_tokens: 32768
        tokenizer: unknown
        usage_reporting: false
`
	result, err := Load(writeTempConfig(t, content))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	primary := result.Config.Models.Definitions["primary"]
	if primary.APIKeyEnv != "ANTHROPIC_API_KEY" || primary.APIKeyFile != "" {
		t.Errorf("primary secret reference = %+v", primary)
	}
	if auxiliary := result.Config.Models.Definitions["auxiliary"]; auxiliary.APIKeyEnv != "" || auxiliary.APIKeyFile != "" {
		t.Errorf("loopback secret reference = %+v", auxiliary)
	}
}

func TestLoad_ModelDefinitionEnvironmentOverride(t *testing.T) {
	content := `version: 1
models:
  definitions:
    primary:
      protocol: anthropic_messages
      model: claude-sonnet
      base_url: https://api.anthropic.com
      api_key_env: ANTHROPIC_API_KEY
      capabilities:
        streaming: true
        tools: true
        structured_output: false
        vision: true
        audio: false
        reasoning: true
        context_tokens: 200000
        tokenizer: anthropic
        usage_reporting: true
`
	t.Setenv("AURA_MODELS_DEFINITIONS_PRIMARY_MODEL", "claude-opus")
	result, err := Load(writeTempConfig(t, content))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := result.Config.Models.Definitions["primary"].Model; got != "claude-opus" {
		t.Errorf("model = %q, want claude-opus", got)
	}
}

func writeTempConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func mustRegistry(t *testing.T, definitions ...capability.Definition) capability.Registry {
	t.Helper()
	registry, err := capability.NewRegistry(definitions...)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	return registry
}

func TestLoad_DuplicateCapabilitiesRejected(t *testing.T) {
	path := writeTempConfig(t, "version: 1\ncapabilities:\n  enabled: [sample, sample]\n")
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), `duplicate capability "sample"`) {
		t.Fatalf("Load duplicate capabilities error = %v", err)
	}
}

func TestLoad_UnknownCapability(t *testing.T) {
	path := writeTempConfig(t, "version: 1\ncapabilities:\n  enabled: [unknown-test]\n")
	_, err := Load(path)
	if code, ok := capability.CodeOf(err); !ok || code != capability.ErrorCodeCapabilityUnknown {
		t.Fatalf("capability.CodeOf(%v) = %q, %v; want %q, true", err, code, ok, capability.ErrorCodeCapabilityUnknown)
	}
}

func TestLoad_EnabledCapabilityAbsentFromBuild(t *testing.T) {
	registry := mustRegistry(t, capability.Definition{
		Name:     "sample",
		Profiles: []capability.Profile{capability.ProfileCore},
	})
	build, err := capability.NewBuild(string(capability.ProfileCore), nil, "linux")
	if err != nil {
		t.Fatalf("NewBuild: %v", err)
	}
	path := writeTempConfig(t, "version: 1\ncapabilities:\n  enabled: [sample]\n")
	_, err = LoadWithOptions(path, LoadOptions{Build: build, Registry: registry})
	if code, ok := capability.CodeOf(err); !ok || code != capability.ErrorCodeCapabilityUnavailable {
		t.Fatalf("capability.CodeOf(%v) = %q, %v; want %q, true", err, code, ok, capability.ErrorCodeCapabilityUnavailable)
	}
}

func TestLoad_MissingDependencyState(t *testing.T) {
	registry := mustRegistry(t, capability.Definition{
		Name:       "sample-runtime",
		Effectful:  true,
		Profiles:   []capability.Profile{capability.ProfileExecLinux},
		Dependency: "sample-host-runtime",
	})
	build, err := capability.NewBuild(string(capability.ProfileExecLinux), []string{"sample-runtime"}, "linux")
	if err != nil {
		t.Fatalf("NewBuild: %v", err)
	}
	options := LoadOptions{Build: build, Registry: registry}

	disabledPath := writeTempConfig(t, "version: 1\ncapabilities:\n  enabled: []\n")
	result, err := LoadWithOptions(disabledPath, options)
	if err != nil {
		t.Fatalf("LoadWithOptions disabled missing dependency: %v", err)
	}
	status, ok := result.CapabilityReport.Status("sample-runtime")
	if !ok || status.State != capability.StateMissing || status.MissingDependency != "sample-host-runtime" {
		t.Fatalf("missing dependency status = %+v, %v", status, ok)
	}

	enabledPath := writeTempConfig(t, "version: 1\ncapabilities:\n  enabled: [sample-runtime]\n")
	_, err = LoadWithOptions(enabledPath, options)
	if code, ok := capability.CodeOf(err); !ok || code != capability.ErrorCodeDependencyMissing {
		t.Fatalf("capability.CodeOf(%v) = %q, %v; want %q, true", err, code, ok, capability.ErrorCodeDependencyMissing)
	}
}

func TestLoad_CapabilitiesEnvironmentOverride(t *testing.T) {
	registry := mustRegistry(t, capability.Definition{
		Name:     "sample",
		Profiles: []capability.Profile{capability.ProfileCore},
	})
	build, err := capability.NewBuild(string(capability.ProfileCore), []string{"sample"}, "linux")
	if err != nil {
		t.Fatalf("NewBuild: %v", err)
	}
	t.Setenv("AURA_CAPABILITIES_ENABLED", "sample")
	path := writeTempConfig(t, "version: 1\ncapabilities:\n  enabled: []\n")
	result, err := LoadWithOptions(path, LoadOptions{Build: build, Registry: registry})
	if err != nil {
		t.Fatalf("LoadWithOptions: %v", err)
	}
	status, ok := result.CapabilityReport.Status("sample")
	if !ok || status.State != capability.StateEnabled {
		t.Fatalf("enabled status = %+v, %v", status, ok)
	}
}

func TestResolvePath_XDGEmptyFallsBack(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("AURA_CONFIG", "")
	home := t.TempDir()
	t.Setenv("HOME", home)

	path, explicit, err := resolvePath("")
	if err != nil {
		t.Fatalf("resolvePath: %v", err)
	}
	if explicit {
		t.Error("explicit = true, want false for default resolution")
	}
	want := filepath.Join(home, ".config", "aura", "config.yaml")
	if path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
}
