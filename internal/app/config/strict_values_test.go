package config

import (
	"reflect"
	"strings"
	"testing"

	"github.com/go-viper/mapstructure/v2"

	"github.com/anggasct/aura/internal/platform/configkit"
)

func TestQuotedNumbersAreRefusedInEverySection(t *testing.T) {
	cases := []struct {
		name     string
		content  string
		path     string
		position string
		detail   string
	}{
		{"integer in tools.exec", "version: 1\ntools:\n  exec:\n    max_stdout_bytes: \"65536\"\n", "tools.exec.max_stdout_bytes", "line 4, column 23", "must be an integer"},
		{"integer in workflows", "version: 1\nworkflows:\n  max_concurrent_steps: \"4\"\n", "workflows.max_concurrent_steps", "line 3, column 25", "must be an integer"},
		{"float in context", "version: 1\ncontext:\n  high_water_ratio: \"0.9\"\n", "context.high_water_ratio", "line 3, column 21", "expected type 'float64', got unconvertible type 'string'"},
		{"boolean in memory", "version: 1\nmemory:\n  enabled: \"true\"\n", "memory.enabled", "line 3, column 12", "expected type 'bool', got unconvertible type 'string'"},
		{"integer in children", "version: 1\nchildren:\n  max_depth: \"1\"\n", "children.max_depth", "line 3, column 14", "must be an integer"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeTempConfig(t, tc.content))
			requireProblemText(t, err, invalidText(tc.path, tc.position, tc.detail))
		})
	}
}

func TestDurationAndByteSizeTakeOnlyStrings(t *testing.T) {
	refused := []struct {
		name     string
		content  string
		path     string
		position string
		detail   string
	}{
		{"a bare duration", "version: 1\nruntime:\n  turn_timeout: 30\n", "runtime.turn_timeout", "line 3, column 17", "must be a duration written as a string"},
		{"a bare byte size", "version: 1\nstorage:\n  artifact_quota: 1048576\n", "storage.artifact_quota", "line 3, column 19", "must be a byte size written as a string"},
	}
	for _, tc := range refused {
		t.Run("refuses "+tc.name, func(t *testing.T) {
			_, err := Load(writeTempConfig(t, tc.content))
			requireProblemText(t, err, invalidText(tc.path, tc.position, tc.detail))
		})
	}
	accepted := []struct {
		name    string
		content string
		check   func(*Config) bool
	}{
		{"a duration string", "version: 1\nruntime:\n  turn_timeout: \"45s\"\n", func(c *Config) bool { return c.Runtime.TurnTimeout == configkit.Duration(45e9) }},
		{"a quoted byte count", "version: 1\nstorage:\n  artifact_quota: \"1048576\"\n", func(c *Config) bool { return c.Storage.ArtifactQuota == 1048576 }},
		{"a byte size with a unit", "version: 1\nstorage:\n  artifact_quota: 1GiB\n", func(c *Config) bool { return c.Storage.ArtifactQuota == 1<<30 }},
	}
	for _, tc := range accepted {
		t.Run("accepts "+tc.name, func(t *testing.T) {
			res, err := Load(writeTempConfig(t, tc.content))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if !tc.check(res.Config) {
				t.Errorf("config = %+v, want the written value", res.Config)
			}
		})
	}
}

func TestNonIntegerNumbersAreRefusedForIntegers(t *testing.T) {
	for _, value := range []string{"4.5", "4.0", "1e6"} {
		t.Run("refuses "+value, func(t *testing.T) {
			_, err := Load(writeTempConfig(t, "version: 1\nruntime:\n  max_active_turns: "+value+"\n"))
			requireProblemText(t, err, invalidText("runtime.max_active_turns", "line 3, column 21", "must be an integer"))
		})
	}
	t.Run("accepts 4", func(t *testing.T) {
		res, err := Load(writeTempConfig(t, "version: 1\nruntime:\n  max_active_turns: 4\n"))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if got := res.Config.Runtime.MaxActiveTurns; got != 4 {
			t.Errorf("max_active_turns = %d, want 4", got)
		}
	})
	t.Run("refuses an integer past the range of its field", func(t *testing.T) {
		_, err := Load(writeTempConfig(t, "version: 1\nusage:\n  daily_budget_micros: 9223372036854775808\n"))
		requireProblemText(t, err, invalidText("usage.daily_budget_micros", "line 3, column 24", "must be between -9223372036854775808 and 9223372036854775807"))
	})
	t.Run("range of every integer kind", func(t *testing.T) {
		cases := []struct {
			name  string
			to    reflect.Type
			input any
			ok    bool
		}{
			{"uint8 at the maximum", reflect.TypeFor[uint8](), 255, true},
			{"uint8 past the maximum", reflect.TypeFor[uint8](), 256, false},
			{"uint16 negative", reflect.TypeFor[uint16](), -1, false},
			{"uint64 at the maximum", reflect.TypeFor[uint64](), uint64(1<<64 - 1), true},
			{"uint64 negative", reflect.TypeFor[uint64](), int64(-1), false},
			{"int8 at the minimum", reflect.TypeFor[int8](), -128, true},
			{"int8 below the minimum", reflect.TypeFor[int8](), -129, false},
			{"int32 past the maximum", reflect.TypeFor[int32](), int64(1 << 31), false},
			{"int64 from a huge unsigned", reflect.TypeFor[int64](), uint64(1 << 63), false},
			{"int from a small unsigned", reflect.TypeFor[int](), uint64(7), true},
			{"int from a float", reflect.TypeFor[int](), 1.0, false},
			{"int from a string", reflect.TypeFor[int](), "1", false},
			{"int from a bool", reflect.TypeFor[int](), true, false},
			{"a string target is left alone", reflect.TypeFor[string](), "x", true},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				_, err := mapstructure.DecodeHookExec(integerValue(), reflect.ValueOf(tc.input), reflect.New(tc.to).Elem())
				if (err == nil) != tc.ok {
					t.Errorf("decode hook(%v, %v) error = %v, want ok %t", tc.input, tc.to, err, tc.ok)
				}
			})
		}
	})
}

func TestAllDecodeProblemsAreReportedTogether(t *testing.T) {
	_, err := loadWithEnvironment(t, "version: 1\nbogus: 1\nserver:\n  port: \"8280\"\n", map[string]string{"AURA_RUNTIME_TURN_TIMEOUT": "soon"})
	if err == nil {
		t.Fatal("Load succeeded, want three problems")
	}
	want := []string{
		unknownKeyText("bogus", "line 2, column 1"),
		invalidText("server.port", "line 4, column 9", "must be an integer"),
		invalidText("runtime.turn_timeout", "env AURA_RUNTIME_TURN_TIMEOUT", "must be a duration"),
	}
	if got := strings.Split(strings.TrimPrefix(err.Error(), "config: "), "\n"); !reflect.DeepEqual(got, want) {
		t.Errorf("problems = %q, want %q", got, want)
	}
}

func TestNullValuesAreRefused(t *testing.T) {
	cases := []struct {
		name     string
		content  string
		path     string
		position string
	}{
		{"an empty section", "version: 1\nserver:\n", "server", "line 2, column 8"},
		{"an empty optional section", "version: 1\ntools:\n", "tools", "line 2, column 7"},
		{"a null scalar", "version: 1\nserver: {port: ~}\n", "server.port", "line 2, column 16"},
		{"a null list item", "version: 1\ncapabilities:\n  enabled:\n    - sample\n    - null\n", "capabilities.enabled[1]", "line 5, column 7"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeTempConfig(t, tc.content))
			requireProblemText(t, err, invalidText(tc.path, tc.position, "must not be null"))
		})
	}
}

func TestScalarsOutsideTheTextNumberAndBoolTagsAreRefused(t *testing.T) {
	cases := []struct {
		name     string
		content  string
		path     string
		position string
	}{
		{"a timestamp scalar", "version: 1\nruntime:\n  turn_timeout: 2026-01-01\n", "runtime.turn_timeout", "line 3, column 17"},
		{"a binary scalar", "version: 1\nlogging:\n  level: !!binary aGVsbG8=\n", "logging.level", "line 3, column 10"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeTempConfig(t, tc.content))
			requireProblemText(t, err, invalidText(tc.path, tc.position, "must be a string, a number or a boolean"))
		})
	}
}

func TestTheDefaultsLayerDecodesToDefault(t *testing.T) {
	file := map[string]any{}
	for _, section := range optionalSections {
		file[section] = map[string]any{}
	}
	layer, err := defaultsLayer(file, nil)
	if err != nil {
		t.Fatalf("defaultsLayer: %v", err)
	}
	var decoded Config
	if err := decodeInput(layer, &decoded); err != nil {
		t.Fatalf("decoding the defaults layer: %v", err)
	}
	want := Default()
	deriveEmptyCollections(&decoded)
	deriveEmptyCollections(&want)
	if !reflect.DeepEqual(decoded, want) {
		t.Errorf("decoded defaults differ from Default():\n got %+v\nwant %+v", decoded, want)
	}
}

func TestWrittenEmptyPersonaValuesAreRefused(t *testing.T) {
	const head = "    - id: kantor\n      description: d\n      instructions: i\n"
	cases := []struct {
		name   string
		fields string
		path   string
		line   string
		code   ErrorCode
		detail string
		clean  string
	}{
		{"an empty name", "      name: \"\"\n", "name", "line 11, column 13", personaInvalidCode, "must not be empty", ""},
		{"an empty evals dir", "      evals:\n        dir: \"\"\n", "evals.dir", "line 12, column 14", invalidCode, "must be a non-empty string", "      evals:\n        behavior:\n          required: true\n"},
		{"an empty judge route", "      evals:\n        behavior:\n          judge_route: \"\"\n", "evals.behavior.judge_route", "line 13, column 24", invalidCode, "must be a non-empty string", "      evals:\n        behavior:\n          required: true\n"},
		{"a blank schedule", "      evals:\n        behavior:\n          schedule: \" \"\n", "evals.behavior.schedule", "line 13, column 21", invalidCode, "must be a non-empty string", "      evals:\n        behavior:\n          required: true\n"},
		{"a zero min score", "      evals:\n        behavior:\n          min_score: 0\n", "evals.behavior.min_score", "line 13, column 22", invalidCode, "must be a number greater than 0 and at most 1", "      evals:\n        behavior:\n          required: true\n"},
		{"a min score above 1", "      evals:\n        behavior:\n          min_score: 1.5\n", "evals.behavior.min_score", "line 13, column 22", invalidCode, "must be a number greater than 0 and at most 1", "      evals:\n        behavior:\n          required: true\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writePersonaConfig(t, "", "", head+tc.fields))
			path := "agents.definitions[0]." + tc.path
			want := path + " (" + tc.line + "): " + string(tc.code) + ": " + tc.detail
			requireProblemText(t, err, want)
			if code, ok := codeOf(err); !ok || code != tc.code {
				t.Errorf("code = %v, %v, want %s (err %v)", code, ok, tc.code, err)
			}
			if _, err := Load(writePersonaConfig(t, "", "", head+tc.clean)); err != nil {
				t.Errorf("the same persona without the key fails: %v", err)
			}
		})
	}
}
