package tools

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func TestCodeExecuteDefinition(t *testing.T) {
	definition, found := DefinitionsByKey()["code_execute@v1"]
	if !found {
		t.Fatal("code_execute@v1 is not in the static catalog")
	}
	if !definition.Effectful || !definition.RequiresApproval {
		t.Errorf("code_execute must be effectful and need approval: %+v", definition)
	}
	if !slices.Equal(definition.RequiredCapabilities, []string{"code-interpreter"}) {
		t.Errorf("required capabilities = %v", definition.RequiredCapabilities)
	}
	if definition.ExternalSink {
		t.Error("code_execute has no network and is not an external sink")
	}
}

func TestCodeExecuteSchemaListsEveryRuntimeAndNothingElse(t *testing.T) {
	var schema struct {
		Properties map[string]struct {
			Enum      []string `json:"enum"`
			MinLength int      `json:"minLength"`
			Type      string   `json:"type"`
		} `json:"properties"`
		Required             []string `json:"required"`
		AdditionalProperties bool     `json:"additionalProperties"`
	}
	if err := json.Unmarshal(codeExecuteSchema(), &schema); err != nil {
		t.Fatalf("schema: %v", err)
	}
	if !slices.Equal(schema.Properties["runtime"].Enum, CodeRuntimes()) || !slices.Equal(CodeRuntimes(), []string{"python", "javascript", "go"}) {
		t.Errorf("runtime enum = %v, runtimes = %v", schema.Properties["runtime"].Enum, CodeRuntimes())
	}
	if len(schema.Properties) != 3 || schema.Properties["source"].MinLength != 1 || schema.Properties["stdin"].Type != "string" {
		t.Errorf("properties = %+v", schema.Properties)
	}
	if !slices.Equal(schema.Required, []string{"runtime", "source"}) || schema.AdditionalProperties {
		t.Errorf("required = %v, additionalProperties = %v", schema.Required, schema.AdditionalProperties)
	}
	for _, forbidden := range []string{"args", "env", "path", "output_files"} {
		if _, present := schema.Properties[forbidden]; present {
			t.Errorf("the schema accepts %q", forbidden)
		}
	}
}

func TestCodeExecuteValidation(t *testing.T) {
	definition := DefinitionsByKey()["code_execute@v1"]
	cases := []struct {
		name string
		raw  string
		ok   bool
	}{
		{"python", `{"runtime":"python","source":"print(1)"}`, true},
		{"javascript with stdin", `{"runtime":"javascript","source":"1","stdin":"in"}`, true},
		{"go is accepted and answered later", `{"runtime":"go","source":"package main"}`, true},
		{"empty stdin", `{"runtime":"python","source":"x","stdin":""}`, true},
		{"unknown runtime", `{"runtime":"ruby","source":"x"}`, false},
		{"runtime case", `{"runtime":"Python","source":"x"}`, false},
		{"missing runtime", `{"source":"x"}`, false},
		{"missing source", `{"runtime":"python"}`, false},
		{"empty source", `{"runtime":"python","source":""}`, false},
		{"numeric source", `{"runtime":"python","source":1}`, false},
		{"numeric stdin", `{"runtime":"python","source":"x","stdin":1}`, false},
		{"null stdin", `{"runtime":"python","source":"x","stdin":null}`, false},
		{"environment", `{"runtime":"python","source":"x","env":{"A":"b"}}`, false},
		{"arguments", `{"runtime":"python","source":"x","args":["a"]}`, false},
		{"output files", `{"runtime":"python","source":"x","output_files":["a.txt"]}`, false},
		{"source above its bound", `{"runtime":"python","source":"` + strings.Repeat("x", MaxCodeSourceBytes+1) + `"}`, false},
		{"source at its bound", `{"runtime":"python","source":"` + strings.Repeat("x", MaxCodeSourceBytes) + `"}`, true},
		{"stdin above its bound", `{"runtime":"python","source":"x","stdin":"` + strings.Repeat("x", MaxCodeStdinBytes+1) + `"}`, false},
		{"not an object", `"python"`, false},
		{"trailing content", `{"runtime":"python","source":"x"}{}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := definition.Validate(json.RawMessage(tc.raw))
			if (err == nil) != tc.ok {
				t.Fatalf("Validate = %v, want ok = %v", err, tc.ok)
			}
			if err != nil && strings.Contains(err.Error(), "xxxx") {
				t.Fatalf("the error echoes the program: %v", err)
			}
			if err == nil && !json.Valid(got) {
				t.Fatalf("canonical arguments are not JSON: %s", got)
			}
		})
	}
}
