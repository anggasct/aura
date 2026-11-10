package effect

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

func normalizeRequest(raw json.RawMessage) (json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()

	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, codedError(ErrorCodeInvalidArgument, "effect: request must be valid JSON", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, codedError(ErrorCodeInvalidArgument, "effect: request must contain one JSON value", nil)
		}
		return nil, codedError(ErrorCodeInvalidArgument, "effect: request has trailing data", err)
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, codedError(ErrorCodeInvalidArgument, "effect: request must be a JSON object", nil)
	}
	if err := validateRequestValue(object, "$", false); err != nil {
		return nil, err
	}
	normalized, err := json.Marshal(object)
	if err != nil {
		return nil, fmt.Errorf("effect: normalize request: %w", err)
	}
	return normalized, nil
}

func validateRequestValue(value any, path string, reference bool) error {
	if reference {
		object, ok := value.(map[string]any)
		if !ok {
			return codedError(ErrorCodeRequestUnsafe, fmt.Sprintf("effect: secret field %s must contain a reference object", path), nil)
		}
		return validateSecretReference(object, path)
	}
	switch value := value.(type) {
	case map[string]any:
		for key, child := range value {
			childPath := path + "." + key
			if isSensitiveRequestKey(key) {
				if err := validateRequestValue(child, childPath, true); err != nil {
					return err
				}
				continue
			}
			if err := validateRequestValue(child, childPath, false); err != nil {
				return err
			}
		}
	case []any:
		for i, child := range value {
			if err := validateRequestValue(child, fmt.Sprintf("%s[%d]", path, i), false); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateSecretReference(value map[string]any, path string) error {
	if len(value) != 1 {
		return codedError(ErrorCodeRequestUnsafe, fmt.Sprintf("effect: secret field %s must contain one reference", path), nil)
	}
	for key, raw := range value {
		if key != "env" && key != "file" {
			return codedError(ErrorCodeRequestUnsafe, fmt.Sprintf("effect: secret field %s must use env or file reference", path), nil)
		}
		ref, ok := raw.(string)
		if !ok || strings.TrimSpace(ref) == "" {
			return codedError(ErrorCodeRequestUnsafe, fmt.Sprintf("effect: secret field %s has an invalid reference", path), nil)
		}
	}
	return nil
}

func isSensitiveRequestKey(key string) bool {
	normalized := strings.ToLower(strings.NewReplacer("-", "_", ".", "_").Replace(key))
	switch normalized {
	case "api_key", "authorization", "cookie", "credential", "password", "private_key", "secret", "token":
		return true
	default:
		return strings.HasSuffix(normalized, "_credential") ||
			strings.HasSuffix(normalized, "_password") ||
			strings.HasSuffix(normalized, "_secret") ||
			strings.HasSuffix(normalized, "_token")
	}
}
