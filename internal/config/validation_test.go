package config

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/platform/capability"
	"github.com/anggasct/aura/internal/platform/configkit"
	"github.com/anggasct/aura/internal/platform/fault"
)

const envCanary = "s05-canary-value"

const testModelDefinition = "models:\n  definitions:\n    primary:\n      protocol: openai_chat_compat\n      model: m\n      base_url: http://127.0.0.1:1\n"

func fieldProblems(err error) []*configkit.FieldError {
	var problems []*configkit.FieldError
	var walk func(err error)
	walk = func(err error) {
		if err == nil {
			return
		}
		if joined, ok := err.(interface{ Unwrap() []error }); ok {
			for _, child := range joined.Unwrap() {
				walk(child)
			}
			return
		}
		node, isField := errors.AsType[*configkit.FieldError](err)
		if isField && node.Error() == err.Error() {
			problems = append(problems, node)
			return
		}
		walk(errors.Unwrap(err))
	}
	walk(err)
	return problems
}

func problemPaths(err error) []string {
	var paths []string
	for _, problem := range fieldProblems(err) {
		paths = append(paths, problem.Path)
	}
	return paths
}

func TestLoadReportsEveryValidationProblem(t *testing.T) {
	content := "version: 1\nruntime:\n  max_active_turns: 0\nserver:\n  port: 70000\ntelemetry:\n  exporter: bogus\n"
	_, err := Load(writeTempConfig(t, content))
	if err == nil {
		t.Fatal("Load accepted a config with three invalid sections")
	}
	problems := fieldProblems(err)
	if len(problems) != 3 {
		t.Fatalf("problems = %v, want three joined problems (err %v)", problemPaths(err), err)
	}
	for i, want := range []string{"runtime.max_active_turns", "server.port", "telemetry.exporter"} {
		if problems[i].Path != want {
			t.Errorf("problem %d path = %q, want %q", i, problems[i].Path, want)
		}
		if !fault.HasCode(problems[i], configkit.ErrorCodeInvalid) {
			t.Errorf("problem %d (%v) does not carry %s", i, problems[i], configkit.ErrorCodeInvalid)
		}
	}
}

func TestEveryValidatorRunsOverInvalidFixtures(t *testing.T) {
	t.Run("fixtures that fail still fail", func(t *testing.T) {
		failing := []string{
			"invalid.yaml",
			"unknown_keys.yaml",
			"provider_colliding_header.yaml",
			"provider_contradictory_protocol.yaml",
			"provider_secret_header.yaml",
			"provider_unknown.yaml",
		}
		for _, name := range failing {
			t.Run(name, func(t *testing.T) {
				if _, err := Load(fixture(t, name)); err == nil {
					t.Fatalf("Load(%s) succeeded, want a failure", name)
				}
			})
		}
		for _, name := range []string{"valid.yaml", "custom_provider.yaml", "provider_reference.yaml"} {
			t.Run(name, func(t *testing.T) {
				if _, err := Load(fixture(t, name)); err != nil {
					t.Fatalf("Load(%s): %v", name, err)
				}
			})
		}
	})

	t.Run("invalid sections beside absent sections", func(t *testing.T) {
		cases := map[string]struct {
			content string
			want    []string
		}{
			"invalid runtime, every optional section absent": {
				content: "version: 1\nruntime:\n  max_active_turns: -1\n",
				want:    []string{"runtime.max_active_turns", "tools", "skills", "context", "profile", "children", "vision"},
			},
			"empty model definition, no routes": {
				content: "version: 1\nmodels:\n  definitions:\n    primary: {}\n",
				want:    []string{"models.definitions.primary", "tools"},
			},
			"route naming a definition that does not exist": {
				content: "version: 1\n" + testModelDefinition + "model_routes:\n  main:\n    candidates: [ghost]\n",
				want:    []string{"model_routes.main", "tools"},
			},
			"telegram enabled without accounts or owner": {
				content: "version: 1\nchannels:\n  telegram:\n    enabled: true\n",
				want:    []string{"channels.telegram.enabled", "tools"},
			},
			"binding to an account that does not exist": {
				content: "version: 1\nbindings:\n  - persona: lobby\n    channel: discord\n    account: ghost\n    guild_id: \"1\"\n",
				want:    []string{"bindings[0].account", "tools"},
			},
			"invalid owner beside absent sections": {
				content: "version: 1\nowner:\n  timezone: Nowhere/Land\n",
				want:    []string{"owner.timezone", "tools"},
			},
			"sync enabled without a remote": {
				content: "version: 1\nsync:\n  enabled: true\n",
				want:    []string{"sync.enabled", "tools"},
			},
			"persona with a missing soul file beside an invalid runtime": {
				content: "version: 1\nruntime:\n  max_active_turns: -1\nagents:\n  definitions:\n    - id: lobby\n      description: d\n      soul_file: missing.md\n",
				want:    []string{"runtime.max_active_turns", "tools"},
			},
		}
		for name, tc := range cases {
			t.Run(name, func(t *testing.T) {
				_, err := LoadWithOptions(writeTempConfig(t, tc.content), execLinuxOptions(t))
				if err == nil {
					t.Fatal("Load accepted an invalid config")
				}
				paths := problemPaths(err)
				for _, want := range tc.want {
					if !slices.Contains(paths, want) {
						t.Errorf("problems %v do not include %q", paths, want)
					}
				}
				for _, path := range paths {
					if strings.HasSuffix(path, ".soul_file") {
						t.Errorf("the soul file was read although validation failed: %v", err)
					}
				}
			})
		}
	})

	t.Run("sections that are absent or empty validate without a panic", func(t *testing.T) {
		for _, content := range []string{
			"version: 1\n",
			"version: 1\nmodels:\n  definitions: {}\nmodel_routes: {}\n",
			"version: 1\nbindings: []\nchannels:\n  discord:\n    accounts: []\n",
			"version: 1\nmcp:\n  servers: []\nplugins:\n  desired: []\n",
		} {
			if _, err := Load(writeTempConfig(t, content)); err != nil {
				t.Errorf("Load(%q): %v", content, err)
			}
		}
	})

	t.Run("a zero config validates without a panic", func(t *testing.T) {
		for _, profile := range []capability.Profile{capability.ProfileCore, capability.ProfileExecLinux} {
			t.Run(string(profile), func(t *testing.T) {
				if err := validateLoaded(&Config{}, true, profile); err == nil {
					t.Fatal("a zero config validated")
				}
			})
		}
	})
}

func TestValidationDetailsNeverQuoteAnEnvironmentValue(t *testing.T) {
	t.Run("environment values", func(t *testing.T) {
		t.Setenv("AURA_LOGGING_LEVEL", envCanary)
		t.Setenv("AURA_LOGGING_FORMAT", envCanary)
		t.Setenv("AURA_TELEMETRY_EXPORTER", envCanary)
		_, err := Load(writeTempConfig(t, "version: 1\n"))
		if err == nil {
			t.Fatal("Load accepted three invalid environment values")
		}
		want := map[string]string{
			"logging.level":      "must be one of debug, info, warn, error",
			"logging.format":     "must be one of text, json",
			"telemetry.exporter": "must be one of none, stdout, otlp_grpc, otlp_http",
		}
		variables := map[string]string{
			"logging.level":      "AURA_LOGGING_LEVEL",
			"logging.format":     "AURA_LOGGING_FORMAT",
			"telemetry.exporter": "AURA_TELEMETRY_EXPORTER",
		}
		problems := fieldProblems(err)
		if len(problems) != len(want) {
			t.Fatalf("problems = %v, want the three environment paths (err %v)", problemPaths(err), err)
		}
		for _, problem := range problems {
			detail, ok := want[problem.Path]
			if !ok {
				t.Errorf("unexpected problem path %q", problem.Path)
				continue
			}
			if !fault.HasCode(problem, configkit.ErrorCodeInvalid) {
				t.Errorf("%s: %v does not carry %s", problem.Path, problem, configkit.ErrorCodeInvalid)
			}
			if got, wantText := problem.Error(), problem.Path+" (env "+variables[problem.Path]+"): config_invalid: "+detail; got != wantText {
				t.Errorf("%s: text = %q, want %q", problem.Path, got, wantText)
			}
		}
		if strings.Contains(err.Error(), envCanary) {
			t.Errorf("error text quotes the environment value: %v", err)
		}
	})

	t.Run("values of leaves the environment can set", func(t *testing.T) {
		cases := map[string]struct {
			content string
			path    string
		}{
			"provider of a definition":       {"version: 1\nmodels:\n  definitions:\n    primary:\n      provider: " + envCanary + "\n      model: m\n", "models.definitions"},
			"protocol of a definition":       {"version: 1\nmodels:\n  definitions:\n    primary:\n      protocol: " + envCanary + "\n      model: m\n", "models.definitions.primary"},
			"base url of a definition":       {"version: 1\nmodels:\n  definitions:\n    primary:\n      protocol: openai_chat_compat\n      model: m\n      base_url: \"://" + envCanary + "\"\n", "models.definitions.primary.base_url"},
			"protocol of a provider preset":  {"version: 1\nmodels:\n  providers:\n    local:\n      protocol: " + envCanary + "\n      base_url: http://127.0.0.1:1\n", "models.providers.local"},
			"base url of a provider preset":  {"version: 1\nmodels:\n  providers:\n    local:\n      protocol: openai_chat_compat\n      base_url: \"://" + envCanary + "\"\n", "models.providers.local"},
			"routing role":                   {"version: 1\nmodels:\n  routing:\n    agent: " + envCanary + "\n", "models.routing.agent"},
			"route candidates":               {"version: 1\n" + testModelDefinition + "model_routes:\n  main:\n    candidates: [" + envCanary + "]\n", "model_routes.main"},
			"sync branch":                    {"version: 1\nsync:\n  branch: \"-" + envCanary + "\"\n", "sync.branch"},
			"sync include":                   {"version: 1\nsync:\n  include: [" + envCanary + "]\n", "sync.include"},
			"sync remote scheme":             {"version: 1\nsync:\n  remote: " + envCanary + "://host/repo.git\n", "sync.remote"},
			"health listen":                  {"version: 1\nhealth:\n  listen: " + envCanary + "\n", "health.listen"},
			"webhook listen address":         {"version: 1\nwebhook:\n  listen_address: " + envCanary + "\n", "webhook.listen_address"},
			"broadcast timezone":             {"version: 1\nbroadcast:\n  timezone: " + envCanary + "\n", "broadcast.timezone"},
			"broadcast quiet hours start":    {"version: 1\nbroadcast:\n  quiet_hours:\n    start: " + envCanary + "\n", "broadcast.quiet_hours.start"},
			"broadcast quiet hours end":      {"version: 1\nbroadcast:\n  quiet_hours:\n    end: " + envCanary + "\n", "broadcast.quiet_hours.end"},
			"broadcast destination route":    {"version: 1\nbroadcast:\n  destinations:\n    ops: \"bad route " + envCanary + "\"\n", "broadcast.destinations[ops]"},
			"scheduler default timezone":     {"version: 1\nscheduler:\n  default_timezone: " + envCanary + "\n", "scheduler.default_timezone"},
			"skills roots":                   {"version: 1\nskills:\n  roots: [" + envCanary + "]\n", "skills.roots"},
			"owner timezone":                 {"version: 1\nowner:\n  timezone: " + envCanary + "\n", "owner.timezone"},
			"knowledge embedding field name": {"version: 1\nknowledge:\n  embedding:\n    base_url: https://embed.example.com/v1\n    model: m\n    dimensions: 8\n    dimensions_field: " + envCanary + "\n", "knowledge.embedding.dimensions_field"},
		}
		for name, tc := range cases {
			t.Run(name, func(t *testing.T) {
				_, err := Load(writeTempConfig(t, tc.content))
				if err == nil {
					t.Fatal("Load accepted the invalid value")
				}
				if !slices.Contains(problemPaths(err), tc.path) {
					t.Errorf("problems %v do not include %q", problemPaths(err), tc.path)
				}
				if strings.Contains(err.Error(), envCanary) {
					t.Errorf("error text quotes the value: %v", err)
				}
			})
		}
	})
}
