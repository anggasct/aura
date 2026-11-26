package filesystem

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/toolbroker"
	"github.com/anggasct/aura/internal/platform/tools"
)

func TestFilesystemToolsMatchBuiltinCatalog(t *testing.T) {
	definitions := ToolDefinitions()
	rules := ToolRules()
	if len(definitions) != 3 {
		t.Fatalf("ToolDefinitions() count = %d, want 3", len(definitions))
	}
	if len(rules) != len(definitions) {
		t.Fatalf("ToolRules() count = %d, want %d", len(rules), len(definitions))
	}

	catalog := tools.DefinitionsByKey()
	policy := toolbroker.DefaultPolicy()

	payloads := map[string][]struct {
		name string
		raw  string
		ok   bool
	}{
		ReadFileToolName: {
			{name: "valid path only", raw: `{"path":"note.txt"}`, ok: true},
			{name: "valid with max_bytes", raw: `{"path":"note.txt","max_bytes":1024}`, ok: true},
			{name: "missing path", raw: `{}`, ok: false},
			{name: "empty path", raw: `{"path":""}`, ok: false},
			{name: "non-string path", raw: `{"path":1}`, ok: false},
			{name: "zero max_bytes", raw: `{"path":"note.txt","max_bytes":0}`, ok: false},
			{name: "negative max_bytes", raw: `{"path":"note.txt","max_bytes":-5}`, ok: false},
			{name: "fractional max_bytes", raw: `{"path":"note.txt","max_bytes":1.5}`, ok: false},
			{name: "string max_bytes", raw: `{"path":"note.txt","max_bytes":"10"}`, ok: false},
			{name: "unknown key", raw: `{"path":"note.txt","extra":true}`, ok: false},
			{name: "duplicate key", raw: `{"path":"a","path":"b"}`, ok: false},
			{name: "trailing data", raw: `{"path":"note.txt"} {}`, ok: false},
			{name: "not object", raw: `[]`, ok: false},
			{name: "invalid json", raw: `{`, ok: false},
		},
		WriteFileToolName: {
			{name: "valid minimal", raw: `{"path":"note.txt","content":"hello"}`, ok: true},
			{name: "valid with overwrite", raw: `{"path":"note.txt","content":"hello","overwrite":true}`, ok: true},
			{name: "missing path", raw: `{"content":"hello"}`, ok: false},
			{name: "empty path", raw: `{"path":"","content":"hello"}`, ok: false},
			{name: "missing content", raw: `{"path":"note.txt"}`, ok: false},
			{name: "empty content", raw: `{"path":"note.txt","content":""}`, ok: false},
			{name: "non-string content", raw: `{"path":"note.txt","content":123}`, ok: false},
			{name: "non-bool overwrite", raw: `{"path":"note.txt","content":"x","overwrite":"yes"}`, ok: false},
			{name: "unknown key", raw: `{"path":"note.txt","content":"x","extra":1}`, ok: false},
			{name: "duplicate key", raw: `{"path":"a","path":"b","content":"x"}`, ok: false},
			{name: "trailing data", raw: `{"path":"a","content":"x"} {}`, ok: false},
			{name: "not object", raw: `"x"`, ok: false},
		},
		ListDirToolName: {
			{name: "valid minimal", raw: `{"path":"."}`, ok: true},
			{name: "valid recursive", raw: `{"path":".","recursive":true}`, ok: true},
			{name: "missing path", raw: `{}`, ok: false},
			{name: "empty path", raw: `{"path":""}`, ok: false},
			{name: "non-bool recursive", raw: `{"path":".","recursive":"true"}`, ok: false},
			{name: "unknown key", raw: `{"path":".","extra":false}`, ok: false},
			{name: "duplicate key", raw: `{"path":".","path":"sub"}`, ok: false},
			{name: "trailing data", raw: `{"path":"."} {}`, ok: false},
			{name: "not object", raw: `123`, ok: false},
		},
	}

	for i := range definitions {
		got := definitions[i]
		t.Run(got.Name, func(t *testing.T) {
			wantPtr, ok := catalog[got.Key()]
			if !ok {
				t.Fatalf("%s missing from builtin catalog", got.Key())
			}
			if got.Validator == nil {
				t.Fatalf("%s Validator must not be nil", got.Key())
			}
			gotCopy := got
			gotCopy.Validator = nil
			wantCopy := *wantPtr
			wantCopy.Validator = nil
			if !reflect.DeepEqual(gotCopy, wantCopy) {
				t.Fatalf("definition mismatch:\n got: %+v\nwant: %+v", gotCopy, wantCopy)
			}

			wantRule, ok := policy.Rules[got.Name]
			if !ok {
				t.Fatalf("%s missing from DefaultPolicy", got.Name)
			}
			if !reflect.DeepEqual(rules[i], wantRule) {
				t.Fatalf("rule mismatch:\n got: %+v\nwant: %+v", rules[i], wantRule)
			}

			for _, tc := range payloads[got.Name] {
				t.Run(tc.name, func(t *testing.T) {
					gotOut, gotErr := got.Validate(json.RawMessage(tc.raw))
					wantOut, wantErr := wantPtr.Validate(json.RawMessage(tc.raw))
					if (gotErr == nil) != tc.ok {
						t.Fatalf("got.Validate(%s) err = %v, want ok=%t", tc.raw, gotErr, tc.ok)
					}
					if (gotErr == nil) != (wantErr == nil) {
						t.Fatalf("parity mismatch for %s: gotErr=%v wantErr=%v", tc.raw, gotErr, wantErr)
					}
					if tc.ok && !bytes.Equal(gotOut, wantOut) {
						t.Fatalf("validated output mismatch: got %s, want %s", gotOut, wantOut)
					}
				})
			}
		})
	}
}

func TestNewRejectsNilDeps(t *testing.T) {
	if _, err := New(nil); !fault.HasCode(err, ErrorCodeInvalidArgument) {
		t.Fatalf("New(nil) err = %v, want code %q", err, ErrorCodeInvalidArgument)
	}
}

func TestNilModuleToolsIsEmpty(t *testing.T) {
	var mod *Module
	if got := mod.Tools(); got != nil {
		t.Fatalf("(*Module)(nil).Tools() = %v, want nil", got)
	}
	var provider *toolProvider
	if got := provider.Tools(); got != nil {
		t.Fatalf("(*toolProvider)(nil).Tools() = %v, want nil", got)
	}
}

func TestNewToolProviderRegistersWithBroker(t *testing.T) {
	stub := func(_ context.Context, _ *toolbroker.ToolRequest, _ approval.Constraints) (toolbroker.ToolResult, error) {
		return toolbroker.ToolResult{Class: toolbroker.ResultOK, Output: json.RawMessage(`{}`)}, nil
	}
	adapters := map[string]toolbroker.Adapter{
		ReadFileToolName + "@" + tools.Version:  stub,
		WriteFileToolName + "@" + tools.Version: stub,
		ListDirToolName + "@" + tools.Version:   stub,
	}
	provider := NewToolProvider(adapters)
	registrations := provider.Tools()
	if len(registrations) != 3 {
		t.Fatalf("registrations = %d, want 3", len(registrations))
	}

	broker, err := toolbroker.New(&toolbroker.Options{
		Policy: approval.Policy{Version: "builtin-tools-v1", Rules: map[string]approval.Rule{}},
	})
	if err != nil {
		t.Fatalf("toolbroker.New: %v", err)
	}
	if err := broker.RegisterProvider(provider); err != nil {
		t.Fatalf("RegisterProvider: %v", err)
	}
	policy := broker.Policy()
	for _, name := range []string{ReadFileToolName, WriteFileToolName, ListDirToolName} {
		if _, ok := policy.Rules[name]; !ok {
			t.Errorf("policy missing rule for %q", name)
		}
	}
}
