package config

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"gopkg.in/yaml.v3"
)

func generatedSchema(t *testing.T) []byte {
	t.Helper()
	got, err := Schema()
	if err != nil {
		t.Fatalf("Schema: %v", err)
	}
	return got
}

func schemaTree(t *testing.T) map[string]any {
	t.Helper()
	var tree map[string]any
	if err := json.Unmarshal(generatedSchema(t), &tree); err != nil {
		t.Fatalf("decode schema: %v", err)
	}
	return tree
}

func resolvedSchema(t *testing.T) *jsonschema.Resolved {
	t.Helper()
	var schema jsonschema.Schema
	if err := json.Unmarshal(generatedSchema(t), &schema); err != nil {
		t.Fatalf("decode schema: %v", err)
	}
	resolved, err := schema.Resolve(nil)
	if err != nil {
		t.Fatalf("resolve schema: %v", err)
	}
	return resolved
}

func jsonInstance(t *testing.T, document []byte) any {
	t.Helper()
	var parsed any
	if err := yaml.Unmarshal(document, &parsed); err != nil {
		t.Fatalf("parse yaml: %v", err)
	}
	encoded, err := json.Marshal(parsed)
	if err != nil {
		t.Fatalf("convert to json: %v", err)
	}
	var instance any
	if err := json.Unmarshal(encoded, &instance); err != nil {
		t.Fatalf("decode json: %v", err)
	}
	return instance
}

func schemaChild(t *testing.T, node map[string]any, key string) map[string]any {
	t.Helper()
	child, ok := node[key].(map[string]any)
	if !ok {
		t.Fatalf("schema has no object at %q", key)
	}
	return child
}

func walkSchemas(node map[string]any, visit func(path string, schema map[string]any)) {
	var walk func(path string, schema map[string]any)
	walk = func(path string, schema map[string]any) {
		visit(path, schema)
		if properties, ok := schema["properties"].(map[string]any); ok {
			for name, child := range properties {
				if childSchema, ok := child.(map[string]any); ok {
					walk(path+"."+name, childSchema)
				}
			}
		}
		for _, key := range []string{"additionalProperties", "items"} {
			if child, ok := schema[key].(map[string]any); ok {
				walk(path+"."+key, child)
			}
		}
	}
	walk("$", node)
}

func TestSchemaGoldenFile(t *testing.T) {
	got := generatedSchema(t)
	goldenPath := filepath.Join(testdataDir, "golden", "config.schema.json")
	if *updateGolden {
		if err := os.WriteFile(goldenPath, got, 0o600); err != nil {
			t.Fatalf("rewrite golden: %v", err)
		}
		return
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("golden mismatch\n--- got ---\n%s--- want ---\n%s", got, want)
	}
}

func TestSchemaHasNoRequiredNoNullAndRefusesExtraProperties(t *testing.T) {
	tree := schemaTree(t)
	if tree["$schema"] != "https://json-schema.org/draft/2020-12/schema" {
		t.Errorf("$schema = %v", tree["$schema"])
	}
	walkSchemas(tree, func(path string, schema map[string]any) {
		if _, present := schema["required"]; present {
			t.Errorf("%s has a required list", path)
		}
		switch declared := schema["type"].(type) {
		case string:
			if declared == "null" {
				t.Errorf("%s has type null", path)
			}
		case []any:
			if slices.Contains(declared, any("null")) {
				t.Errorf("%s has a type union with null: %v", path, declared)
			}
		}
	})
	properties := schemaChild(t, tree, "properties")
	if tree["additionalProperties"] != false {
		t.Errorf("root additionalProperties = %v, want false", tree["additionalProperties"])
	}
	server := schemaChild(t, properties, "server")
	if server["additionalProperties"] != false {
		t.Errorf("server additionalProperties = %v, want false", server["additionalProperties"])
	}
	if tools := schemaChild(t, properties, "tools"); tools["type"] != "object" {
		t.Errorf("tools type = %v, want object", tools["type"])
	}
}

func TestSchemaAcceptsTheGoldenDefaultFile(t *testing.T) {
	resolved := resolvedSchema(t)
	golden, err := os.ReadFile(filepath.Join(testdataDir, "golden", "config.yaml"))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	valid, err := os.ReadFile(fixture(t, "valid.yaml"))
	if err != nil {
		t.Fatalf("read valid fixture: %v", err)
	}
	unknownKeys, err := os.ReadFile(fixture(t, "unknown_keys.yaml"))
	if err != nil {
		t.Fatalf("read unknown keys fixture: %v", err)
	}
	cases := []struct {
		name     string
		document []byte
		accepted bool
	}{
		{name: "golden default file", document: golden, accepted: true},
		{name: "valid fixture", document: valid, accepted: true},
		{name: "unknown key", document: unknownKeys, accepted: false},
		{name: "quoted number for an integer", document: []byte("server:\n  port: \"8280\"\n"), accepted: false},
		{name: "number for a duration", document: []byte("runtime:\n  turn_timeout: 30\n"), accepted: false},
		{name: "malformed duration", document: []byte("runtime:\n  turn_timeout: soon\n"), accepted: false},
		{name: "number for a byte size", document: []byte("storage:\n  artifact_quota: 1073741824\n"), accepted: false},
		{name: "byte size string", document: []byte("storage:\n  artifact_quota: 512MiB\n"), accepted: true},
		{name: "secret reference with another scheme", document: []byte("telemetry:\n  credential_ref: vault://x\n"), accepted: false},
		{name: "environment secret reference", document: []byte("telemetry:\n  credential_ref: env://TOKEN\n"), accepted: true},
		{name: "null section", document: []byte("tools:\n"), accepted: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := resolved.Validate(jsonInstance(t, tc.document))
			if tc.accepted && err != nil {
				t.Fatalf("schema refused a document it must accept: %v", err)
			}
			if !tc.accepted && err == nil {
				t.Fatal("schema accepted a document it must refuse")
			}
		})
	}
}
