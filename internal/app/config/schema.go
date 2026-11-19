package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/anggasct/aura/internal/platform/configkit"
	"github.com/anggasct/aura/internal/platform/secret"
)

const (
	schemaDialect   = "https://json-schema.org/draft/2020-12/schema"
	durationPattern = `^[-+]?(0|(([0-9]+(\.[0-9]*)?|\.[0-9]+)(ns|us|µs|μs|ms|s|m|h))+)$`
	byteSizePattern = `^\s*\+?[0-9]+\s*(KiB|MiB|GiB|TiB|KB|MB|GB|TB)?\s*$`
	refPattern      = `^(|env://[A-Za-z_][A-Za-z0-9_]*|file:///.*)$`
	nullType        = "null"
)

func Schema() ([]byte, error) {
	schema, err := jsonschema.For[Config](&jsonschema.ForOptions{TypeSchemas: schemaTypeSchemas()})
	if err != nil {
		return nil, fmt.Errorf("build config schema: %w", err)
	}
	schema.Schema = schemaDialect
	relax(schema)
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(schema); err != nil {
		return nil, fmt.Errorf("write config schema: %w", err)
	}
	return buf.Bytes(), nil
}

func schemaTypeSchemas() map[reflect.Type]*jsonschema.Schema {
	return map[reflect.Type]*jsonschema.Schema{
		reflect.TypeFor[configkit.Duration](): {Type: "string", Pattern: durationPattern},
		reflect.TypeFor[configkit.ByteSize](): {Type: "string", Pattern: byteSizePattern},
		reflect.TypeFor[secret.Ref]():         {Type: "string", Pattern: refPattern},
	}
}

func relax(schema *jsonschema.Schema) {
	if schema == nil {
		return
	}
	schema.Required = nil
	if len(schema.Types) > 0 {
		schema.Types = slices.DeleteFunc(schema.Types, func(t string) bool { return t == nullType })
		if len(schema.Types) == 1 {
			schema.Type, schema.Types = schema.Types[0], nil
		}
	}
	for _, property := range schema.Properties {
		relax(property)
	}
	relax(schema.AdditionalProperties)
	relax(schema.Items)
}
