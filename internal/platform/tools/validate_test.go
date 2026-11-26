package tools

import (
	"encoding/json"
	"testing"
)

func TestDecodeObjectRejectsNonObjectsAndTrailingData(t *testing.T) {
	object, err := DecodeObject(json.RawMessage(`{"name":"aura","count":3}`))
	if err != nil {
		t.Fatalf("DecodeObject valid object: %v", err)
	}
	if object["name"] != "aura" {
		t.Fatalf("name = %v, want aura", object["name"])
	}
	if num, ok := object["count"].(json.Number); !ok || num.String() != "3" {
		t.Fatalf("count = %#v, want json.Number(3)", object["count"])
	}

	cases := map[string]string{
		"invalid json":   `{`,
		"array":          `["a"]`,
		"scalar":         `"hello"`,
		"null":           `null`,
		"trailing value": `{"a":1} {"b":2}`,
		"trailing junk":  `{"a":1} trailing`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeObject(json.RawMessage(raw)); err == nil {
				t.Fatalf("DecodeObject(%s) accepted invalid input", raw)
			}
		})
	}
}

func TestValidationPrimitives(t *testing.T) {
	t.Run("reject unknown", func(t *testing.T) {
		if err := RejectUnknown(map[string]any{"a": 1}, "a", "b"); err != nil {
			t.Fatalf("RejectUnknown allowed key: %v", err)
		}
		if err := RejectUnknown(map[string]any{"a": 1, "c": 2}, "a", "b"); err == nil {
			t.Fatal("RejectUnknown accepted unknown key")
		}
	})

	t.Run("validate string", func(t *testing.T) {
		if err := ValidateString(map[string]any{"k": "v"}, "k"); err != nil {
			t.Fatalf("ValidateString valid: %v", err)
		}
		for name, value := range map[string]map[string]any{
			"missing": {},
			"empty":   {"k": ""},
			"wrong":   {"k": 123},
		} {
			if err := ValidateString(value, "k"); err == nil {
				t.Errorf("%s: ValidateString accepted invalid input", name)
			}
		}
	})

	t.Run("validate string array", func(t *testing.T) {
		if err := ValidateStringArray(map[string]any{}, "cmd", false); err != nil {
			t.Fatalf("optional missing array: %v", err)
		}
		if err := ValidateStringArray(map[string]any{"cmd": []any{"ls", "-la"}}, "cmd", true); err != nil {
			t.Fatalf("valid array: %v", err)
		}
		for name, value := range map[string]map[string]any{
			"missing required": {},
			"empty array":      {"cmd": []any{}},
			"not an array":     {"cmd": "ls"},
			"empty item":       {"cmd": []any{"ls", ""}},
			"non-string item":  {"cmd": []any{"ls", 1}},
		} {
			if err := ValidateStringArray(value, "cmd", true); err == nil {
				t.Errorf("%s: ValidateStringArray accepted invalid input", name)
			}
		}
	})

	t.Run("validate optional bool", func(t *testing.T) {
		if err := ValidateOptionalBool(map[string]any{}, "flag"); err != nil {
			t.Fatalf("missing optional bool: %v", err)
		}
		if err := ValidateOptionalBool(map[string]any{"flag": true}, "flag"); err != nil {
			t.Fatalf("valid bool: %v", err)
		}
		if err := ValidateOptionalBool(map[string]any{"flag": "true"}, "flag"); err == nil {
			t.Fatal("ValidateOptionalBool accepted non-bool")
		}
	})

	t.Run("validate optional integer", func(t *testing.T) {
		if err := ValidateOptionalInteger(map[string]any{}, "n", 1); err != nil {
			t.Fatalf("missing optional int: %v", err)
		}
		if err := ValidateOptionalInteger(map[string]any{"n": json.Number("5")}, "n", 1); err != nil {
			t.Fatalf("valid int: %v", err)
		}
		for name, value := range map[string]map[string]any{
			"below minimum": {"n": json.Number("0")},
			"float":         {"n": json.Number("1.5")},
			"string":        {"n": "5"},
		} {
			if err := ValidateOptionalInteger(value, "n", 1); err == nil {
				t.Errorf("%s: ValidateOptionalInteger accepted invalid input", name)
			}
		}
	})
}
