package tools

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBuiltinsExposeVersionedSchemas(t *testing.T) {
	definitions := Builtins()
	if len(definitions) != 6 {
		t.Fatalf("built-in definitions = %d, want 6", len(definitions))
	}
	for _, definition := range definitions {
		var schema map[string]any
		if err := json.Unmarshal(definition.Schema, &schema); err != nil {
			t.Fatalf("schema %s: %v", definition.Key(), err)
		}
		if schema["$schema"] != "https://json-schema.org/draft/2020-12/schema" {
			t.Errorf("schema %s has unexpected dialect", definition.Key())
		}
	}
}

func TestDefinitionValidationIsStrictAndCanonical(t *testing.T) {
	definition := DefinitionsByKey()["read_file@v1"]
	got, err := definition.Validate(json.RawMessage(`{"path":"/tmp/file"}`))
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if string(got) != `{"path":"/tmp/file"}` {
		t.Fatalf("canonical arguments = %s", got)
	}
	for _, raw := range []string{`{"path":"/tmp/file","extra":true}`, `{"path":1}`, `[]`} {
		if _, err := definition.Validate(json.RawMessage(raw)); err == nil {
			t.Errorf("Validate(%s) accepted invalid arguments", raw)
		}
	}
}

func TestExecRequiresCommandArray(t *testing.T) {
	definition := DefinitionsByKey()["exec@v1"]
	_, err := definition.Validate(json.RawMessage(`{"command":[]}`))
	if err == nil || !strings.Contains(err.Error(), "non-empty") {
		t.Fatalf("Validate error = %v", err)
	}
}
