package toolbroker

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/tools"
)

func fixtureDefinitions() []tools.Definition {
	return []tools.Definition{
		{
			Name: "exec", Version: tools.Version,
			Schema:               []byte(`{"$schema":"https://json-schema.org/draft/2020-12/schema","$id":"aura://tools/exec/v1","type":"object","properties":{"command":{"type":"array","items":{"type":"string"},"minItems":1},"shell":{"type":"boolean"}},"required":["command"],"additionalProperties":false}`),
			RequiredCapabilities: []string{"exec-linux"}, RequiresApproval: true, Effectful: true,
			Validator: func(raw json.RawMessage) (json.RawMessage, error) {
				v, err := tools.DecodeObject(raw)
				if err != nil {
					return nil, err
				}
				if err := tools.RejectUnknown(v, "command", "shell"); err != nil {
					return nil, err
				}
				if err := tools.ValidateStringArray(v, "command", true); err != nil {
					return nil, err
				}
				if err := tools.ValidateOptionalBool(v, "shell"); err != nil {
					return nil, err
				}
				return json.Marshal(v)
			},
		},
		{
			Name: "code_execute", Version: tools.Version,
			Schema:               []byte(`{"type":"object"}`),
			RequiredCapabilities: []string{"code-interpreter"}, RequiresApproval: true, Effectful: true,
			Validator: func(raw json.RawMessage) (json.RawMessage, error) {
				v, err := tools.DecodeObject(raw)
				if err != nil {
					return nil, err
				}
				return json.Marshal(v)
			},
		},
		{
			Name: "read_file", Version: tools.Version,
			Schema:               []byte(`{"$schema":"https://json-schema.org/draft/2020-12/schema","$id":"aura://tools/read_file/v1","type":"object","properties":{"path":{"type":"string","minLength":1},"max_bytes":{"type":"integer","minimum":1}},"required":["path"],"additionalProperties":false}`),
			RequiredCapabilities: []string{"workspace-read"},
			Validator: func(raw json.RawMessage) (json.RawMessage, error) {
				v, err := tools.DecodeObject(raw)
				if err != nil {
					return nil, err
				}
				if err := tools.RejectUnknown(v, "path", "max_bytes"); err != nil {
					return nil, err
				}
				if err := tools.ValidateString(v, "path"); err != nil {
					return nil, err
				}
				if err := tools.ValidateOptionalInteger(v, "max_bytes", 1); err != nil {
					return nil, err
				}
				return json.Marshal(v)
			},
		},
		{
			Name: "write_file", Version: tools.Version,
			Schema:               []byte(`{"$schema":"https://json-schema.org/draft/2020-12/schema","$id":"aura://tools/write_file/v1","type":"object","properties":{"path":{"type":"string","minLength":1},"content":{"type":"string"},"overwrite":{"type":"boolean"}},"required":["path","content"],"additionalProperties":false}`),
			RequiredCapabilities: []string{"workspace-write"}, RequiresApproval: true, Effectful: true,
			Validator: func(raw json.RawMessage) (json.RawMessage, error) {
				v, err := tools.DecodeObject(raw)
				if err != nil {
					return nil, err
				}
				if err := tools.RejectUnknown(v, "path", "content", "overwrite"); err != nil {
					return nil, err
				}
				if err := tools.ValidateString(v, "path"); err != nil {
					return nil, err
				}
				if err := tools.ValidateString(v, "content"); err != nil {
					return nil, err
				}
				if err := tools.ValidateOptionalBool(v, "overwrite"); err != nil {
					return nil, err
				}
				return json.Marshal(v)
			},
		},
		{
			Name: "list_dir", Version: tools.Version,
			Schema:               []byte(`{"$schema":"https://json-schema.org/draft/2020-12/schema","$id":"aura://tools/list_dir/v1","type":"object","properties":{"path":{"type":"string","minLength":1},"recursive":{"type":"boolean"}},"required":["path"],"additionalProperties":false}`),
			RequiredCapabilities: []string{"workspace-read"},
			Validator: func(raw json.RawMessage) (json.RawMessage, error) {
				v, err := tools.DecodeObject(raw)
				if err != nil {
					return nil, err
				}
				if err := tools.RejectUnknown(v, "path", "recursive"); err != nil {
					return nil, err
				}
				if err := tools.ValidateString(v, "path"); err != nil {
					return nil, err
				}
				if err := tools.ValidateOptionalBool(v, "recursive"); err != nil {
					return nil, err
				}
				return json.Marshal(v)
			},
		},
		{
			Name: "web_fetch", Version: tools.Version,
			Schema:               []byte(`{"$schema":"https://json-schema.org/draft/2020-12/schema","$id":"aura://tools/web_fetch/v1","type":"object","properties":{"url":{"type":"string","format":"uri","minLength":1}},"required":["url"],"additionalProperties":false}`),
			RequiredCapabilities: []string{"public-web"}, ExternalSink: true,
			Validator: func(raw json.RawMessage) (json.RawMessage, error) {
				v, err := tools.DecodeObject(raw)
				if err != nil {
					return nil, err
				}
				if err := tools.RejectUnknown(v, "url"); err != nil {
					return nil, err
				}
				if err := tools.ValidateString(v, "url"); err != nil {
					return nil, err
				}
				return json.Marshal(v)
			},
		},
		{
			Name: "web_search", Version: tools.Version,
			Schema:               []byte(`{"$schema":"https://json-schema.org/draft/2020-12/schema","$id":"aura://tools/web_search/v1","type":"object","properties":{"query":{"type":"string","minLength":1},"max_results":{"type":"integer","minimum":1}},"required":["query"],"additionalProperties":false}`),
			RequiredCapabilities: []string{"provider-search"}, ExternalSink: true,
			Validator: func(raw json.RawMessage) (json.RawMessage, error) {
				v, err := tools.DecodeObject(raw)
				if err != nil {
					return nil, err
				}
				if err := tools.RejectUnknown(v, "query", "max_results"); err != nil {
					return nil, err
				}
				if err := tools.ValidateString(v, "query"); err != nil {
					return nil, err
				}
				if err := tools.ValidateOptionalInteger(v, "max_results", 1); err != nil {
					return nil, err
				}
				return json.Marshal(v)
			},
		},
		{
			Name: tools.KBSearchToolName, Version: tools.Version,
			Schema: []byte(`{"type":"object"}`),
			Validator: func(raw json.RawMessage) (json.RawMessage, error) {
				v, err := tools.DecodeObject(raw)
				if err != nil {
					return nil, err
				}
				if err := tools.RejectUnknown(v, "query", "limit"); err != nil {
					return nil, err
				}
				if err := tools.ValidateString(v, "query"); err != nil {
					return nil, err
				}
				return json.Marshal(v)
			},
		},
		{
			Name: "handoff_to_owner", Version: tools.Version,
			Schema: []byte(`{"type":"object"}`), OptIn: true, Orchestration: true,
			Validator: func(raw json.RawMessage) (json.RawMessage, error) {
				v, err := tools.DecodeObject(raw)
				if err != nil {
					return nil, err
				}
				if err := tools.RejectUnknown(v, "reason"); err != nil {
					return nil, err
				}
				if err := tools.ValidateString(v, "reason"); err != nil {
					return nil, err
				}
				return json.Marshal(v)
			},
		},
		{
			Name: "email_send", Version: tools.Version, Channel: principal.ChannelEmail,
			Schema: []byte(`{"type":"object"}`), Effectful: true, OptIn: true, Display: []string{"to", "subject", "body"},
			Validator: func(raw json.RawMessage) (json.RawMessage, error) {
				v, err := tools.DecodeObject(raw)
				if err != nil {
					return nil, err
				}
				if err := tools.RejectUnknown(v, "to", "subject", "body"); err != nil {
					return nil, err
				}
				for _, key := range []string{"to", "subject", "body"} {
					if err := tools.ValidateString(v, key); err != nil {
						return nil, err
					}
				}
				rawTo, _ := v["to"].(string)
				to := strings.ToLower(strings.TrimSpace(rawTo))
				if !principal.IsPlainEmailAddress(to) {
					return nil, fmt.Errorf("argument %q must be exactly one plain email address", "to")
				}
				v["to"] = to
				return json.Marshal(v)
			},
		},
		{
			Name: "workflow_list", Version: tools.Version, Schema: []byte(`{"type":"object"}`), OptIn: true, Orchestration: true,
			Validator: func(raw json.RawMessage) (json.RawMessage, error) {
				v, err := tools.DecodeObject(raw)
				if err != nil {
					return nil, err
				}
				if err := tools.RejectUnknown(v); err != nil {
					return nil, err
				}
				return json.Marshal(v)
			},
		},
		{
			Name: "workflow_start", Version: tools.Version, Schema: []byte(`{"type":"object"}`), OptIn: true, Orchestration: true, ExternalSink: true,
			Validator: func(raw json.RawMessage) (json.RawMessage, error) {
				v, err := tools.DecodeObject(raw)
				if err != nil {
					return nil, err
				}
				if err := tools.RejectUnknown(v, "id", "objective"); err != nil {
					return nil, err
				}
				if err := tools.ValidateString(v, "id"); err != nil {
					return nil, err
				}
				if objective, ok := v["objective"]; ok {
					text, isString := objective.(string)
					if !isString || utf8.RuneCountInString(text) > 2000 {
						return nil, fmt.Errorf("argument %q must be a string", "objective")
					}
				}
				return json.Marshal(v)
			},
		},
		{
			Name: "workflow_status", Version: tools.Version, Schema: []byte(`{"type":"object"}`), OptIn: true, Orchestration: true,
			Validator: func(raw json.RawMessage) (json.RawMessage, error) {
				v, err := tools.DecodeObject(raw)
				if err != nil {
					return nil, err
				}
				if err := tools.RejectUnknown(v, "run_id"); err != nil {
					return nil, err
				}
				if err := tools.ValidateString(v, "run_id"); err != nil {
					return nil, err
				}
				return json.Marshal(v)
			},
		},
		{
			Name: "workflow_propose", Version: tools.Version, Schema: []byte(`{"type":"object"}`), OptIn: true, Orchestration: true, ExternalSink: true,
			Validator: func(raw json.RawMessage) (json.RawMessage, error) {
				v, err := tools.DecodeObject(raw)
				if err != nil {
					return nil, err
				}
				if err := tools.RejectUnknown(v, "spec"); err != nil {
					return nil, err
				}
				if spec, ok := v["spec"].(map[string]any); !ok || len(spec) == 0 {
					return nil, fmt.Errorf("argument %q must be a workflow spec object", "spec")
				}
				return json.Marshal(v)
			},
		},
		{
			Name: "workflow_save_routine", Version: tools.Version, Schema: []byte(`{"type":"object"}`), OptIn: true, Orchestration: true,
			Validator: func(raw json.RawMessage) (json.RawMessage, error) {
				v, err := tools.DecodeObject(raw)
				if err != nil {
					return nil, err
				}
				if err := tools.RejectUnknown(v, "run_id", "name"); err != nil {
					return nil, err
				}
				if err := tools.ValidateString(v, "run_id"); err != nil {
					return nil, err
				}
				if err := tools.ValidateString(v, "name"); err != nil {
					return nil, err
				}
				return json.Marshal(v)
			},
		},
		{
			Name: "workflow_cancel", Version: tools.Version, Schema: []byte(`{"type":"object"}`), OptIn: true, Orchestration: true,
			Validator: func(raw json.RawMessage) (json.RawMessage, error) {
				v, err := tools.DecodeObject(raw)
				if err != nil {
					return nil, err
				}
				if err := tools.RejectUnknown(v, "run_id"); err != nil {
					return nil, err
				}
				if err := tools.ValidateString(v, "run_id"); err != nil {
					return nil, err
				}
				return json.Marshal(v)
			},
		},
	}
}

func fixtureDefinition(name string) *tools.Definition {
	defs := fixtureDefinitions()
	for i := range defs {
		if defs[i].Name == name {
			def := defs[i]
			return &def
		}
	}
	return nil
}

func fixturePolicy() approval.Policy {
	defs := fixtureDefinitions()
	rules := make(map[string]approval.Rule, len(defs))
	for i := range defs {
		rules[defs[i].Name] = DefaultRule(&defs[i])
	}
	return approval.Policy{Version: "builtin-tools-v1", Rules: rules}
}

func newFixtureBroker(t *testing.T, options *Options) *Broker {
	t.Helper()
	broker, err := newFixtureBrokerOrErr(t, options)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return broker
}

func newFixtureBrokerOrErr(t *testing.T, options *Options) (*Broker, error) {
	t.Helper()
	var opts Options
	if options != nil {
		opts = *options
	}
	if opts.Policy.Version == "" {
		opts.Policy = fixturePolicy()
	}
	broker, err := New(&opts)
	if err != nil {
		return nil, err
	}
	defs := fixtureDefinitions()
	for i := range defs {
		def := cloneDefinition(&defs[i])
		broker.definitions[def.Key()] = &def
	}
	return broker, nil
}
