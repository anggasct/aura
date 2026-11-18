package config

import (
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/configkit"
	"github.com/anggasct/aura/internal/platform/fault"
)

func loadWithEnvironment(t *testing.T, content string, environment map[string]string) (LoadResult, error) {
	t.Helper()
	for name, value := range environment {
		t.Setenv(name, value)
	}
	return Load(writeTempConfig(t, content))
}

type environmentProblem struct {
	path   string
	name   string
	detail string
	value  string
}

func requireEnvironmentProblem(t *testing.T, err error, problem environmentProblem) {
	t.Helper()
	if err == nil {
		t.Fatalf("Load succeeded, want %s (env %s) to fail", problem.path, problem.name)
	}
	want := problem.path + " (env " + problem.name + "): config_invalid: " + problem.detail
	if got := strings.TrimPrefix(err.Error(), "config: "); got != want {
		t.Errorf("error = %q, want %q", got, want)
	}
	if strings.Contains(err.Error(), problem.value) {
		t.Errorf("error text echoes the environment value: %v", err)
	}
	problems := fieldProblems(err)
	if len(problems) != 1 {
		t.Fatalf("problems = %v, want exactly one", problems)
	}
	if code, ok := fault.CodeOf[configkit.ErrorCode](problems[0]); !ok || code != configkit.ErrorCodeInvalid {
		t.Errorf("code = %q, %v; want %q", code, ok, configkit.ErrorCodeInvalid)
	}
}

func TestEnvPathPrefersTheLongestFieldSuffix(t *testing.T) {
	t.Setenv("AURA_MODELS_DEFINITIONS_PRIMARY_CUSTOM_BASE_URL", "https://custom.example.com")
	t.Setenv("AURA_MODELS_DEFINITIONS_PRIMARY_BASE_URL", "https://base.example.com")
	cfg, err := decode([]byte("version: 1\n"))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	primary, found := cfg.Models.Definitions["primary"]
	if !found {
		t.Fatalf("definitions = %v, want primary", cfg.Models.Definitions)
	}
	if primary.Custom == nil || primary.Custom.BaseURL != "https://custom.example.com" {
		t.Errorf("primary custom = %+v, want base_url https://custom.example.com", primary.Custom)
	}
	if primary.BaseURL != "https://base.example.com" {
		t.Errorf("primary base_url = %q, want https://base.example.com", primary.BaseURL)
	}
	if _, stray := cfg.Models.Definitions["primary_custom"]; stray {
		t.Errorf("definitions = %v, want no primary_custom entry", cfg.Models.Definitions)
	}
}

func TestEnvValueIsParsedForItsFieldType(t *testing.T) {
	refused := []struct {
		name   string
		value  string
		path   string
		detail string
	}{
		{"AURA_SERVER_PORT", envCanary, "server.port", "must be an integer"},
		{"AURA_RUNTIME_MAX_ACTIVE_TURNS", "99999999999999999999", "runtime.max_active_turns", "must be an integer"},
		{"AURA_RUNTIME_TURN_TIMEOUT", envCanary, "runtime.turn_timeout", "must be a duration"},
		{"AURA_STORAGE_ARTIFACT_QUOTA", envCanary, "storage.artifact_quota", "must be a byte size"},
		{"AURA_CHANNELS_DISCORD_ENABLED", "maybe", "channels.discord.enabled", "must be a boolean"},
		{"AURA_CONTEXT_HIGH_WATER_RATIO", envCanary, "context.high_water_ratio", "must be a number"},
		{"AURA_SKILLS_ROOTS", "a," + envCanary + ",,b", "skills.roots", "must be a list of strings without empty items"},
		{"AURA_VISION_PROVIDERS_FOO_MAX_IMAGES", envCanary, "vision.providers.foo.max_images", "must be an integer"},
	}
	for _, tc := range refused {
		t.Run("refuses "+tc.name, func(t *testing.T) {
			_, err := loadWithEnvironment(t, "version: 1\n", map[string]string{tc.name: tc.value})
			requireEnvironmentProblem(t, err, environmentProblem{path: tc.path, name: tc.name, detail: tc.detail, value: tc.value})
		})
	}

	t.Run("joins every environment problem sorted by variable name", func(t *testing.T) {
		_, err := loadWithEnvironment(t, "version: 1\n", map[string]string{
			"AURA_SERVER_PORT":          envCanary,
			"AURA_RUNTIME_TURN_TIMEOUT": envCanary,
		})
		paths := problemPaths(err)
		if want := []string{"runtime.turn_timeout", "server.port"}; !slices.Equal(paths, want) {
			t.Errorf("problem paths = %v, want %v", paths, want)
		}
	})

	t.Run("hands a parsed value to the decoder", func(t *testing.T) {
		t.Setenv("AURA_SERVER_PORT", "7777")
		t.Setenv("AURA_CHANNELS_DISCORD_ENABLED", "true")
		t.Setenv("AURA_CONTEXT_HIGH_WATER_RATIO", "0.5")
		t.Setenv("AURA_RUNTIME_TURN_TIMEOUT", "45s")
		t.Setenv("AURA_STORAGE_ARTIFACT_QUOTA", "2MiB")
		t.Setenv("AURA_SKILLS_ROOTS", " one , two ")
		cfg, err := decode([]byte("version: 1\n"))
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if cfg.Server.Port != 7777 || !cfg.Channels.Discord.Enabled || cfg.Context.HighWaterRatio != 0.5 {
			t.Errorf("scalars = %d %v %v, want 7777 true 0.5", cfg.Server.Port, cfg.Channels.Discord.Enabled, cfg.Context.HighWaterRatio)
		}
		if time.Duration(cfg.Runtime.TurnTimeout) != 45*time.Second || int64(cfg.Storage.ArtifactQuota) != 2<<20 {
			t.Errorf("text values = %v %v, want 45s 2MiB", cfg.Runtime.TurnTimeout, cfg.Storage.ArtifactQuota)
		}
		if want := []string{"one", "two"}; !slices.Equal(cfg.Skills.Roots, want) {
			t.Errorf("roots = %v, want %v", cfg.Skills.Roots, want)
		}
	})

	t.Run("reads an empty value as an empty list", func(t *testing.T) {
		t.Setenv("AURA_SKILLS_ROOTS", "")
		cfg, err := decode([]byte("version: 1\nskills:\n  roots: [kept]\n"))
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if cfg.Skills.Roots == nil || len(cfg.Skills.Roots) != 0 {
			t.Errorf("roots = %#v, want an empty list", cfg.Skills.Roots)
		}
	})

	t.Run("sets one leaf and keeps its siblings", func(t *testing.T) {
		t.Setenv("AURA_VISION_PROVIDERS_FOO_MAX_IMAGES", "3")
		cfg, err := decode([]byte("version: 1\nvision:\n  providers:\n    foo:\n      detail: low\n    bar:\n      detail: high\n"))
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		providers := cfg.Vision.Providers
		if providers["foo"].MaxImages != 3 || providers["foo"].Detail != "low" || providers["bar"].Detail != "high" {
			t.Errorf("providers = %+v, want foo max_images 3 detail low and bar detail high", providers)
		}
	})
}

func TestEnvInsideAListItemFailsTheLoad(t *testing.T) {
	refused := []struct {
		name string
		path string
	}{
		{"AURA_AGENTS_DEFINITIONS_ID", "agents.definitions.id"},
		{"AURA_CHANNELS_DISCORD_ACCOUNTS_NAME", "channels.discord.accounts.name"},
		{"AURA_CHANNELS_DISCORD_ACCOUNTS", "channels.discord.accounts"},
		{"AURA_SERVER", "server"},
	}
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadWithEnvironment(t, "version: 1\n", map[string]string{tc.name: envCanary})
			requireEnvironmentProblem(t, err, environmentProblem{path: tc.path, name: tc.name, detail: "is not addressable from the environment", value: envCanary})
		})
	}

	t.Run("warns about a name that is no config path", func(t *testing.T) {
		res, err := loadWithEnvironment(t, "version: 1\n", map[string]string{"AURA_SERVER_PORTX": "1"})
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if want := []string{"AURA_SERVER_PORTX"}; !slices.Equal(res.Warnings, want) {
			t.Errorf("warnings = %v, want %v", res.Warnings, want)
		}
	})
}

func TestEnvNameBelowAMapKeyThatReachesNoLeafWarns(t *testing.T) {
	warned := []string{
		"AURA_MODELS_DEFINITIONS_PRIMARY_API_KEY",
		"AURA_MODELS_DEFINITIONS_PRIMARY",
		"AURA_VISION_PROVIDERS_FOO",
		"AURA_MODELS_PROVIDERS_FOO_HEADERS_X",
		"AURA_MCP_SERVERS_ENVIRONMENT_X",
		"AURA_MODELS_ROUTING_AGENT",
	}
	for _, name := range warned {
		t.Run(name, func(t *testing.T) {
			res, err := loadWithEnvironment(t, "version: 1\n", map[string]string{name: "x"})
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if want := []string{name}; !slices.Equal(res.Warnings, want) {
				t.Errorf("warnings = %v, want %v", res.Warnings, want)
			}
		})
	}

	t.Run("fails for a whole map", func(t *testing.T) {
		_, err := loadWithEnvironment(t, "version: 1\n", map[string]string{"AURA_MODELS_DEFINITIONS": envCanary})
		requireEnvironmentProblem(t, err, environmentProblem{path: "models.definitions", name: "AURA_MODELS_DEFINITIONS", detail: "is not addressable from the environment", value: envCanary})
	})
}

func TestEnvNamedBySecretReferenceIsNotWarned(t *testing.T) {
	cases := []struct {
		name    string
		content string
		set     string
	}{
		{
			name:    "default sync secret reference",
			content: "version: 1\nsync:\n  enabled: false\n",
			set:     "AURA_SYNC_TRANSPORT_SECRET",
		},
		{
			name:    "mcp oauth client id",
			content: "version: 1\nmcp:\n  servers:\n    - name: remote\n      transport: streamable_http\n      url: https://mcp.example.com/mcp\n      auth:\n        oauth:\n          client_id_env: AURA_TEST_CLIENT_ID\n          client_secret_env: AURA_TEST_CLIENT_SECRET\n          token_store: keyring\n      capabilities: [\"external.read\"]\n",
			set:     "AURA_TEST_CLIENT_ID",
		},
		{
			name:    "model api key variable",
			content: "version: 1\n" + testModelDefinition + "      api_key_env: AURA_TEST_MODEL_KEY\n" + providerTestCapabilities,
			set:     "AURA_TEST_MODEL_KEY",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := loadWithEnvironment(t, tc.content, map[string]string{tc.set: "secret", "AURA_UNCONFIGURED_SECRET": "secret"})
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if want := []string{"AURA_UNCONFIGURED_SECRET"}; !slices.Equal(res.Warnings, want) {
				t.Errorf("warnings = %v, want only %v", res.Warnings, want)
			}
		})
	}
}

func TestEnvIndexHasNoCollisions(t *testing.T) {
	if _, err := buildIndex(reflect.TypeFor[Config]()); err != nil {
		t.Fatalf("buildIndex over the root type: %v", err)
	}
	type colliding struct {
		Underscore string `json:"a_b"`
		Dotted     string `json:"a.b"`
	}
	_, err := buildIndex(reflect.TypeFor[colliding]())
	if err == nil {
		t.Fatal("buildIndex over a type with a_b and a.b succeeded, want a collision error")
	}
	for _, path := range []string{"a_b", "a.b"} {
		if !strings.Contains(err.Error(), path) {
			t.Errorf("collision error %q does not name %q", err, path)
		}
	}
}
