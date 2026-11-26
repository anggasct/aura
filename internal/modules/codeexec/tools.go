package codeexec

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/toolbroker"
	"github.com/anggasct/aura/internal/platform/tools"
)

const (
	ToolName       = "code_execute"
	CapabilityName = "code-interpreter"

	MaxCodeSourceBytes = 1 << 20
	MaxCodeStdinBytes  = 16 << 20

	MaxCodeOutputFiles     = 128
	MaxCodeOutputNameBytes = 64
	MaxCodeOutputDepth     = 4
	codeOutputNamePattern  = `^[A-Za-z0-9._-]+(/[A-Za-z0-9._-]+){0,3}$`
)

func CodeRuntimes() []string {
	return []string{"python", "javascript", "go"}
}

func ToolDefinition() tools.Definition {
	return tools.Definition{
		Name:                 ToolName,
		Version:              tools.Version,
		Schema:               codeExecuteSchema(),
		RequiredCapabilities: []string{CapabilityName},
		RequiresApproval:     true,
		Effectful:            true,
		Validator:            validateToolArguments,
	}
}

func ToolRule() approval.Rule {
	definition := ToolDefinition()
	return toolbroker.DefaultRule(&definition)
}

func codeExecuteSchema() []byte {
	runtimes := CodeRuntimes()
	enum := make([]string, 0, len(runtimes))
	for _, runtime := range runtimes {
		enum = append(enum, strconv.Quote(runtime))
	}
	return []byte(`{"$schema":"https://json-schema.org/draft/2020-12/schema","$id":"aura://tools/code_execute/v1","type":"object","properties":{"runtime":{"type":"string","enum":[` +
		strings.Join(enum, ",") +
		`]},"source":{"type":"string","minLength":1},"stdin":{"type":"string"},"output_files":{"type":"array","description":"Relative file names written under the output/ directory of the working directory. Each existing regular file is imported as an artifact.","maxItems":` +
		strconv.Itoa(MaxCodeOutputFiles) +
		`,"uniqueItems":true,"items":{"type":"string","maxLength":` +
		strconv.Itoa(MaxCodeOutputNameBytes) +
		`,"pattern":"` + codeOutputNamePattern + `"}}},"required":["runtime","source"],"additionalProperties":false}`)
}

func validateToolArguments(raw json.RawMessage) (json.RawMessage, error) {
	value, err := tools.DecodeObject(raw)
	if err != nil {
		return nil, err
	}
	if err := tools.RejectUnknown(value, "runtime", "source", "stdin", "output_files"); err != nil {
		return nil, err
	}
	if err := tools.ValidateString(value, "runtime"); err != nil {
		return nil, err
	}
	if runtime, _ := value["runtime"].(string); !slices.Contains(CodeRuntimes(), runtime) {
		return nil, fmt.Errorf("argument %q must be one of %v", "runtime", CodeRuntimes())
	}
	if err := tools.ValidateString(value, "source"); err != nil {
		return nil, err
	}
	if source, _ := value["source"].(string); len(source) > MaxCodeSourceBytes {
		return nil, fmt.Errorf("argument %q must be at most %d bytes", "source", MaxCodeSourceBytes)
	}
	if rawStdin, present := value["stdin"]; present {
		stdin, ok := rawStdin.(string)
		if !ok {
			return nil, fmt.Errorf("argument %q must be a string", "stdin")
		}
		if len(stdin) > MaxCodeStdinBytes {
			return nil, fmt.Errorf("argument %q must be at most %d bytes", "stdin", MaxCodeStdinBytes)
		}
	}
	if rawFiles, present := value["output_files"]; present {
		if err := validateCodeOutputFilesValue(rawFiles); err != nil {
			return nil, err
		}
	}
	return json.Marshal(value)
}

func validateCodeOutputFilesValue(raw any) error {
	items, ok := raw.([]any)
	if !ok {
		return fmt.Errorf("argument %q must be an array of strings", "output_files")
	}
	if len(items) > MaxCodeOutputFiles {
		return fmt.Errorf("argument %q must list at most %d files", "output_files", MaxCodeOutputFiles)
	}
	names := make([]string, len(items))
	for i, item := range items {
		name, ok := item.(string)
		if !ok {
			return fmt.Errorf("argument %q must be an array of strings", "output_files")
		}
		names[i] = name
	}
	return ValidateCodeOutputFiles(names, MaxCodeOutputFiles)
}

func ValidateCodeOutputFiles(names []string, limit int) error {
	if len(names) > limit {
		return fmt.Errorf("argument %q must list at most %d files", "output_files", limit)
	}
	seen := make(map[string]struct{}, len(names))
	for _, name := range names {
		if err := ValidateCodeOutputFile(name); err != nil {
			return err
		}
		if _, duplicate := seen[name]; duplicate {
			return fmt.Errorf("argument %q must not repeat a file", "output_files")
		}
		seen[name] = struct{}{}
	}
	return nil
}

func ValidateCodeOutputFile(name string) error {
	invalid := fmt.Errorf("argument %q must hold relative names of letters, digits, dots, dashes and underscores", "output_files")
	if name == "" || len(name) > MaxCodeOutputNameBytes {
		return invalid
	}
	segments := strings.Split(name, "/")
	if len(segments) > MaxCodeOutputDepth {
		return invalid
	}
	for _, segment := range segments {
		if segment == "" || segment == "." || segment == ".." {
			return invalid
		}
		for i := range len(segment) {
			if !codeOutputByte(segment[i]) {
				return invalid
			}
		}
	}
	return nil
}

func codeOutputByte(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '.' || b == '_' || b == '-'
}
