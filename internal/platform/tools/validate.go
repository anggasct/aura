package tools

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
)

func DecodeObject(raw json.RawMessage) (map[string]any, error) {
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

func RejectUnknown(value map[string]any, allowed ...string) error {
	for key := range value {
		if !slices.Contains(allowed, key) {
			return fmt.Errorf("unknown argument %q", key)
		}
	}
	return nil
}

func ValidateString(value map[string]any, key string) error {
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

func ValidateStringArray(value map[string]any, key string, required bool) error {
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
		element, ok := item.(string)
		if !ok || element == "" {
			return fmt.Errorf("argument %q must contain only strings", key)
		}
	}
	return nil
}

func ValidateOptionalBool(value map[string]any, key string) error {
	if raw, ok := value[key]; ok {
		if _, ok := raw.(bool); !ok {
			return fmt.Errorf("argument %q must be a boolean", key)
		}
	}
	return nil
}

func ValidateOptionalInteger(value map[string]any, key string, minimum int64) error {
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
