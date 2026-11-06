package tools

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/anggasct/aura/internal/principal"
)

const MCPToolPrefix = "mcp_"

func IsMCPEntry(name string) bool {
	return strings.HasPrefix(name, MCPToolPrefix)
}

func MCPServerEntry(server string) string {
	return MCPToolPrefix + server
}

const (
	PluginToolPrefix     = "plugin_"
	MaxPluginToolWireLen = 64
)

func IsPluginEntry(name string) bool {
	return strings.HasPrefix(name, PluginToolPrefix)
}

func PluginEntry(plugin string) string {
	return PluginToolPrefix + plugin
}

func PluginToolID(plugin, tool string) string {
	return plugin + "." + tool
}

func PluginToolWireName(plugin, tool string) string {
	return PluginToolPrefix + plugin + "_" + tool
}

func NamesPluginTool(entry, plugin string) bool {
	prefix := PluginEntry(plugin) + "_"
	return strings.HasPrefix(entry, prefix) && len(entry) > len(prefix)
}

func ListsPluginTool(entries []string, name, plugin string) bool {
	if slices.Contains(entries, name) {
		return true
	}
	return plugin != "" && slices.Contains(entries, PluginEntry(plugin))
}

func ListsTool(entries []string, name, server string) bool {
	if slices.Contains(entries, name) {
		return true
	}
	return server != "" && slices.Contains(entries, MCPServerEntry(server))
}

const (
	Version         = "v1"
	HandoffToolName = "handoff_to_owner"

	EmailSendToolName = "email_send"
	EmailChannel      = "email"

	MaxEmailSubjectBytes = 160
	MaxEmailBodyBytes    = 65536

	WorkflowListToolName        = "workflow_list"
	WorkflowStartToolName       = "workflow_start"
	WorkflowStatusToolName      = "workflow_status"
	WorkflowCancelToolName      = "workflow_cancel"
	WorkflowProposeToolName     = "workflow_propose"
	WorkflowSaveRoutineToolName = "workflow_save_routine"

	MaxWorkflowObjectiveRunes = 2000
)

type Definition struct {
	Name                 string
	Version              string
	Server               string
	Plugin               string
	Description          string
	Schema               json.RawMessage
	RequiredCapabilities []string
	RequiresApproval     bool
	Effectful            bool
	OptIn                bool
	Validator            func(raw json.RawMessage) (json.RawMessage, error)
}

func (d *Definition) Key() string {
	return d.Name + "@" + d.Version
}

func (d *Definition) Validate(raw json.RawMessage) (json.RawMessage, error) {
	if d.Validator != nil {
		return d.Validator(raw)
	}
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
	case WorkflowListToolName:
		if err := rejectUnknown(value); err != nil {
			return nil, err
		}
	case WorkflowStartToolName:
		if err := rejectUnknown(value, "id", "objective"); err != nil {
			return nil, err
		}
		if err := validateString(value, "id"); err != nil {
			return nil, err
		}
		if objective, ok := value["objective"]; ok {
			text, isString := objective.(string)
			if !isString || utf8.RuneCountInString(text) > MaxWorkflowObjectiveRunes {
				return nil, fmt.Errorf("argument %q must be a string of at most %d characters", "objective", MaxWorkflowObjectiveRunes)
			}
		}
	case WorkflowSaveRoutineToolName:
		if err := rejectUnknown(value, "run_id", "name"); err != nil {
			return nil, err
		}
		if err := validateString(value, "run_id"); err != nil {
			return nil, err
		}
		if err := validateString(value, "name"); err != nil {
			return nil, err
		}
	case WorkflowProposeToolName:
		if err := rejectUnknown(value, "spec"); err != nil {
			return nil, err
		}
		if spec, ok := value["spec"].(map[string]any); !ok || len(spec) == 0 {
			return nil, fmt.Errorf("argument %q must be a workflow spec object", "spec")
		}
	case WorkflowStatusToolName, WorkflowCancelToolName:
		if err := rejectUnknown(value, "run_id"); err != nil {
			return nil, err
		}
		if err := validateString(value, "run_id"); err != nil {
			return nil, err
		}
	case HandoffToolName:
		if err := rejectUnknown(value, "reason"); err != nil {
			return nil, err
		}
		if err := validateString(value, "reason"); err != nil {
			return nil, err
		}
	case EmailSendToolName:
		return validateEmailSend(value)
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
		{Name: "exec", Version: Version, Schema: []byte(`{"$schema":"https://json-schema.org/draft/2020-12/schema","$id":"aura://tools/exec/v1","type":"object","properties":{"command":{"type":"array","items":{"type":"string"},"minItems":1},"shell":{"type":"boolean"}},"required":["command"],"additionalProperties":false}`), RequiredCapabilities: []string{"exec-linux"}, RequiresApproval: true, Effectful: true},
		{Name: "read_file", Version: Version, Schema: []byte(`{"$schema":"https://json-schema.org/draft/2020-12/schema","$id":"aura://tools/read_file/v1","type":"object","properties":{"path":{"type":"string","minLength":1},"max_bytes":{"type":"integer","minimum":1}},"required":["path"],"additionalProperties":false}`), RequiredCapabilities: []string{"workspace-read"}},
		{Name: "write_file", Version: Version, Schema: []byte(`{"$schema":"https://json-schema.org/draft/2020-12/schema","$id":"aura://tools/write_file/v1","type":"object","properties":{"path":{"type":"string","minLength":1},"content":{"type":"string"},"overwrite":{"type":"boolean"}},"required":["path","content"],"additionalProperties":false}`), RequiredCapabilities: []string{"workspace-write"}, RequiresApproval: true, Effectful: true},
		{Name: "list_dir", Version: Version, Schema: []byte(`{"$schema":"https://json-schema.org/draft/2020-12/schema","$id":"aura://tools/list_dir/v1","type":"object","properties":{"path":{"type":"string","minLength":1},"recursive":{"type":"boolean"}},"required":["path"],"additionalProperties":false}`), RequiredCapabilities: []string{"workspace-read"}},
		{Name: "web_fetch", Version: Version, Schema: []byte(`{"$schema":"https://json-schema.org/draft/2020-12/schema","$id":"aura://tools/web_fetch/v1","type":"object","properties":{"url":{"type":"string","format":"uri","minLength":1}},"required":["url"],"additionalProperties":false}`), RequiredCapabilities: []string{"public-web"}},
		{Name: "web_search", Version: Version, Schema: []byte(`{"$schema":"https://json-schema.org/draft/2020-12/schema","$id":"aura://tools/web_search/v1","type":"object","properties":{"query":{"type":"string","minLength":1},"max_results":{"type":"integer","minimum":1}},"required":["query"],"additionalProperties":false}`), RequiredCapabilities: []string{"provider-search"}},
		{Name: HandoffToolName, Version: Version, Schema: []byte(`{"$schema":"https://json-schema.org/draft/2020-12/schema","$id":"aura://tools/handoff_to_owner/v1","type":"object","properties":{"reason":{"type":"string","minLength":1,"maxLength":500}},"required":["reason"],"additionalProperties":false}`), OptIn: true},
		{Name: EmailSendToolName, Version: Version, Schema: []byte(`{"$schema":"https://json-schema.org/draft/2020-12/schema","$id":"aura://tools/email_send/v1","type":"object","properties":{"to":{"type":"string","minLength":3,"maxLength":254},"subject":{"type":"string","minLength":1,"maxLength":160},"body":{"type":"string","minLength":1,"maxLength":65536}},"required":["to","subject","body"],"additionalProperties":false}`), Effectful: true, OptIn: true},
		{Name: WorkflowListToolName, Version: Version, Schema: []byte(`{"$schema":"https://json-schema.org/draft/2020-12/schema","$id":"aura://tools/workflow_list/v1","type":"object","properties":{},"additionalProperties":false}`), OptIn: true},
		{Name: WorkflowStartToolName, Version: Version, Schema: []byte(`{"$schema":"https://json-schema.org/draft/2020-12/schema","$id":"aura://tools/workflow_start/v1","type":"object","properties":{"id":{"type":"string","minLength":1},"objective":{"type":"string","maxLength":2000}},"required":["id"],"additionalProperties":false}`), OptIn: true},
		{Name: WorkflowStatusToolName, Version: Version, Schema: []byte(`{"$schema":"https://json-schema.org/draft/2020-12/schema","$id":"aura://tools/workflow_status/v1","type":"object","properties":{"run_id":{"type":"string","minLength":1}},"required":["run_id"],"additionalProperties":false}`), OptIn: true},
		{Name: WorkflowProposeToolName, Version: Version, Schema: []byte(`{"$schema":"https://json-schema.org/draft/2020-12/schema","$id":"aura://tools/workflow_propose/v1","type":"object","properties":{"spec":{"type":"object","properties":{"goal":{"type":"string","minLength":1},"steps":{"type":"array","minItems":1}},"required":["goal","steps"]}},"required":["spec"],"additionalProperties":false}`), OptIn: true},
		{Name: WorkflowSaveRoutineToolName, Version: Version, Schema: []byte(`{"$schema":"https://json-schema.org/draft/2020-12/schema","$id":"aura://tools/workflow_save_routine/v1","type":"object","properties":{"run_id":{"type":"string","minLength":1},"name":{"type":"string","minLength":1,"maxLength":64}},"required":["run_id","name"],"additionalProperties":false}`), OptIn: true},
		{Name: WorkflowCancelToolName, Version: Version, Schema: []byte(`{"$schema":"https://json-schema.org/draft/2020-12/schema","$id":"aura://tools/workflow_cancel/v1","type":"object","properties":{"run_id":{"type":"string","minLength":1}},"required":["run_id"],"additionalProperties":false}`), OptIn: true},
	}
}

func validateEmailSend(value map[string]any) (json.RawMessage, error) {
	if err := rejectUnknown(value, "to", "subject", "body"); err != nil {
		return nil, err
	}
	for _, key := range []string{"to", "subject", "body"} {
		if err := validateString(value, key); err != nil {
			return nil, err
		}
	}
	rawTo, _ := value["to"].(string)
	subject, _ := value["subject"].(string)
	body, _ := value["body"].(string)
	to := strings.ToLower(strings.TrimSpace(rawTo))
	if !principal.IsPlainEmailAddress(to) {
		return nil, fmt.Errorf("argument %q must be exactly one plain email address", "to")
	}
	value["to"] = to
	if len(subject) > MaxEmailSubjectBytes {
		return nil, fmt.Errorf("argument %q must be at most %d bytes", "subject", MaxEmailSubjectBytes)
	}
	if len(body) > MaxEmailBodyBytes {
		return nil, fmt.Errorf("argument %q must be at most %d bytes", "body", MaxEmailBodyBytes)
	}
	return json.Marshal(value)
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

func ChannelOf(name string) string {
	if name == EmailSendToolName {
		return EmailChannel
	}
	return ""
}

func EffectTier(name string) bool {
	for _, definition := range Builtins() {
		if definition.Name == name {
			return definition.Effectful
		}
	}
	return true
}
