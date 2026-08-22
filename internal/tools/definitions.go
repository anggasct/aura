package tools

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
)

const Version = "v1"

type Definition struct {
	Name                 string
	Version              string
	Schema               json.RawMessage
	RequiredCapabilities []string
	RequiresApproval     bool
}

func (d *Definition) Key() string {
	return d.Name + "@" + d.Version
}

func (d *Definition) Validate(raw json.RawMessage) (json.RawMessage, error) {
	value, err := decodeObject(raw)
	if err != nil {
		return nil, err
	}
	switch d.Name {
	case "exec":
		if err := rejectUnknown(value, "command", "shell"); err != nil {
			return nil, err
		}
		if err := validateStringArray(value, "command", true); err != nil {
			return nil, err
		}
		if err := validateOptionalBool(value, "shell"); err != nil {
			return nil, err
		}
	case "read_file":
		if err := rejectUnknown(value, "path", "max_bytes"); err != nil {
			return nil, err
		}
		if err := validateString(value, "path"); err != nil {
			return nil, err
		}
		if err := validateOptionalInteger(value, "max_bytes", 1); err != nil {
			return nil, err
		}
	case "write_file":
		if err := rejectUnknown(value, "path", "content", "overwrite"); err != nil {
			return nil, err
		}
		if err := validateString(value, "path"); err != nil {
			return nil, err
		}
		if err := validateString(value, "content"); err != nil {
			return nil, err
		}
		if err := validateOptionalBool(value, "overwrite"); err != nil {
			return nil, err
		}
	case "list_dir":
		if err := rejectUnknown(value, "path", "recursive"); err != nil {
			return nil, err
		}
		if err := validateString(value, "path"); err != nil {
			return nil, err
		}
		if err := validateOptionalBool(value, "recursive"); err != nil {
			return nil, err
		}
	case "web_fetch":
		if err := rejectUnknown(value, "url"); err != nil {
			return nil, err
		}
		if err := validateString(value, "url"); err != nil {
			return nil, err
		}
	case "web_search":
		if err := rejectUnknown(value, "query", "max_results"); err != nil {
			return nil, err
		}
		if err := validateString(value, "query"); err != nil {
			return nil, err
		}
		if err := validateOptionalInteger(value, "max_results", 1); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("unknown tool %q", d.Name)
	}
	return json.Marshal(value)
}

func Builtins() []Definition {
	return []Definition{
		{Name: "exec", Version: Version, Schema: []byte(`{"$schema":"https://json-schema.org/draft/2020-12/schema","$id":"aura://tools/exec/v1","type":"object","properties":{"command":{"type":"array","items":{"type":"string"},"minItems":1},"shell":{"type":"boolean"}},"required":["command"],"additionalProperties":false}`), RequiredCapabilities: []string{"exec-linux"}, RequiresApproval: true},
		{Name: "read_file", Version: Version, Schema: []byte(`{"$schema":"https://json-schema.org/draft/2020-12/schema","$id":"aura://tools/read_file/v1","type":"object","properties":{"path":{"type":"string","minLength":1},"max_bytes":{"type":"integer","minimum":1}},"required":["path"],"additionalProperties":false}`), RequiredCapabilities: []string{"workspace-read"}},
		{Name: "write_file", Version: Version, Schema: []byte(`{"$schema":"https://json-schema.org/draft/2020-12/schema","$id":"aura://tools/write_file/v1","type":"object","properties":{"path":{"type":"string","minLength":1},"content":{"type":"string"},"overwrite":{"type":"boolean"}},"required":["path","content"],"additionalProperties":false}`), RequiredCapabilities: []string{"workspace-write"}, RequiresApproval: true},
		{Name: "list_dir", Version: Version, Schema: []byte(`{"$schema":"https://json-schema.org/draft/2020-12/schema","$id":"aura://tools/list_dir/v1","type":"object","properties":{"path":{"type":"string","minLength":1},"recursive":{"type":"boolean"}},"required":["path"],"additionalProperties":false}`), RequiredCapabilities: []string{"workspace-read"}},
		{Name: "web_fetch", Version: Version, Schema: []byte(`{"$schema":"https://json-schema.org/draft/2020-12/schema","$id":"aura://tools/web_fetch/v1","type":"object","properties":{"url":{"type":"string","format":"uri","minLength":1}},"required":["url"],"additionalProperties":false}`), RequiredCapabilities: []string{"public-web"}},
		{Name: "web_search", Version: Version, Schema: []byte(`{"$schema":"https://json-schema.org/draft/2020-12/schema","$id":"aura://tools/web_search/v1","type":"object","properties":{"query":{"type":"string","minLength":1},"max_results":{"type":"integer","minimum":1}},"required":["query"],"additionalProperties":false}`), RequiredCapabilities: []string{"provider-search"}},
	}
}

func decodeObject(raw json.RawMessage) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("arguments must be valid JSON: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("arguments must contain one JSON value")
		}
		return nil, fmt.Errorf("arguments have trailing data: %w", err)
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, errors.New("arguments must be a JSON object")
	}
	return object, nil
}

func rejectUnknown(value map[string]any, allowed ...string) error {
	for key := range value {
		if !slices.Contains(allowed, key) {
			return fmt.Errorf("unknown argument %q", key)
		}
	}
	return nil
}

func validateString(value map[string]any, key string) error {
	raw, ok := value[key]
	if !ok {
		return fmt.Errorf("argument %q is required", key)
	}
	s, ok := raw.(string)
	if !ok || s == "" {
		return fmt.Errorf("argument %q must be a non-empty string", key)
	}
	return nil
}

func validateStringArray(value map[string]any, key string, required bool) error {
	raw, ok := value[key]
	if !ok {
		if required {
			return fmt.Errorf("argument %q is required", key)
		}
		return nil
	}
	items, ok := raw.([]any)
	if !ok || len(items) == 0 {
		return fmt.Errorf("argument %q must be a non-empty string array", key)
	}
	for _, item := range items {
		value, ok := item.(string)
		if !ok || value == "" {
			return fmt.Errorf("argument %q must contain only strings", key)
		}
	}
	return nil
}

func validateOptionalBool(value map[string]any, key string) error {
	if raw, ok := value[key]; ok {
		if _, ok := raw.(bool); !ok {
			return fmt.Errorf("argument %q must be a boolean", key)
		}
	}
	return nil
}

func validateOptionalInteger(value map[string]any, key string, minimum int64) error {
	raw, ok := value[key]
	if !ok {
		return nil
	}
	number, ok := raw.(json.Number)
	if !ok {
		return fmt.Errorf("argument %q must be an integer", key)
	}
	valueInt, err := number.Int64()
	if err != nil || valueInt < minimum {
		return fmt.Errorf("argument %q must be at least %d", key, minimum)
	}
	return nil
}

func DefinitionsByKey() map[string]*Definition {
	definitions := Builtins()
	result := make(map[string]*Definition, len(definitions))
	for _, definition := range definitions {
		definition.RequiredCapabilities = slices.Clone(definition.RequiredCapabilities)
		definition.Schema = slices.Clone(definition.Schema)
		definitionCopy := definition
		result[definitionCopy.Key()] = &definitionCopy
	}
	return result
}
