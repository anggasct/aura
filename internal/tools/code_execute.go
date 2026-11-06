package tools

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

const (
	MaxCodeSourceBytes = 1 << 20
	MaxCodeStdinBytes  = 16 << 20
)

func codeExecuteSchema() []byte {
	enum := make([]string, 0, len(CodeRuntimes()))
	for _, runtime := range CodeRuntimes() {
		enum = append(enum, strconv.Quote(runtime))
	}
	return []byte(`{"$schema":"https://json-schema.org/draft/2020-12/schema","$id":"aura://tools/code_execute/v1","type":"object","properties":{"runtime":{"type":"string","enum":[` +
		strings.Join(enum, ",") +
		`]},"source":{"type":"string","minLength":1},"stdin":{"type":"string"}},"required":["runtime","source"],"additionalProperties":false}`)
}

func CodeRuntimes() []string {
	return []string{"python", "javascript", "go"}
}

func validateCodeExecute(value map[string]any) (json.RawMessage, error) {
	if err := rejectUnknown(value, "runtime", "source", "stdin"); err != nil {
		return nil, err
	}
	if err := validateString(value, "runtime"); err != nil {
		return nil, err
	}
	if runtime, _ := value["runtime"].(string); !slices.Contains(CodeRuntimes(), runtime) {
		return nil, fmt.Errorf("argument %q must be one of %v", "runtime", CodeRuntimes())
	}
	if err := validateString(value, "source"); err != nil {
		return nil, err
	}
	if source, _ := value["source"].(string); len(source) > MaxCodeSourceBytes {
		return nil, fmt.Errorf("argument %q must be at most %d bytes", "source", MaxCodeSourceBytes)
	}
	if raw, present := value["stdin"]; present {
		stdin, ok := raw.(string)
		if !ok {
			return nil, fmt.Errorf("argument %q must be a string", "stdin")
		}
		if len(stdin) > MaxCodeStdinBytes {
			return nil, fmt.Errorf("argument %q must be at most %d bytes", "stdin", MaxCodeStdinBytes)
		}
	}
	return json.Marshal(value)
}
