package shellexec

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/toolbroker"
	"github.com/anggasct/aura/internal/platform/tools"
)

func TestShellexecToolMatchesBuiltinCatalog(t *testing.T) {
	wantPtr, ok := tools.DefinitionsByKey()[ToolName+"@"+tools.Version]
	if !ok {
		t.Fatal("exec@v1 missing from builtin catalog")
	}
	got := ToolDefinition()
	if got.Validator == nil {
		t.Fatal("ToolDefinition().Validator must not be nil")
	}
	if !got.RequiresApproval || !got.Effectful {
		t.Fatalf("ToolDefinition() = %+v, want RequiresApproval and Effectful", got)
	}
	gotCopy := got
	gotCopy.Validator = nil
	wantCopy := *wantPtr
	wantCopy.Validator = nil
	if !reflect.DeepEqual(gotCopy, wantCopy) {
		t.Fatalf("ToolDefinition() non-func fields differ from catalog:\n got: %+v\nwant: %+v", gotCopy, wantCopy)
	}

	payloads := []struct {
		name string
		raw  string
		ok   bool
	}{
		{name: "valid command", raw: `{"command":["ls","-la"]}`, ok: true},
		{name: "valid with shell", raw: `{"command":["echo hi"],"shell":true}`, ok: true},
		{name: "missing command", raw: `{}`, ok: false},
		{name: "empty command array", raw: `{"command":[]}`, ok: false},
		{name: "empty command item", raw: `{"command":[""]}`, ok: false},
		{name: "non-string command item", raw: `{"command":[123]}`, ok: false},
		{name: "string command", raw: `{"command":"ls"}`, ok: false},
		{name: "non-bool shell", raw: `{"command":["ls"],"shell":"yes"}`, ok: false},
		{name: "unknown key", raw: `{"command":["ls"],"extra":1}`, ok: false},
		{name: "duplicate key", raw: `{"command":["a"],"command":["b"]}`, ok: false},
		{name: "trailing data", raw: `{"command":["ls"]} {}`, ok: false},
		{name: "not object", raw: `["ls"]`, ok: false},
		{name: "invalid json", raw: `{`, ok: false},
	}
	for _, tc := range payloads {
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

	wantRule, ok := toolbroker.DefaultPolicy().Rules[ToolName]
	if !ok {
		t.Fatal("exec missing from DefaultPolicy")
	}
	if gotRule := ToolRule(); !reflect.DeepEqual(gotRule, wantRule) {
		t.Fatalf("ToolRule() = %+v, want %+v", gotRule, wantRule)
	}
}

func TestNewModuleAndToolProvider(t *testing.T) {
	if _, err := New(nil); !fault.HasCode(err, ErrorCodeInvalidArgument) {
		t.Fatalf("New(nil) err = %v, want code %q", err, ErrorCodeInvalidArgument)
	}
	if _, err := New(&Deps{Workspace: "relative", Config: Default()}); err == nil {
		t.Fatal("New accepted relative workspace")
	}
	if _, err := New(&Deps{Workspace: t.TempDir()}); err == nil {
		t.Fatal("New accepted zero config")
	}

	var nilMod *Module
	if got := nilMod.Tools(); got != nil {
		t.Fatalf("(*Module)(nil).Tools() = %v, want nil", got)
	}
	if got := NewToolProvider(nil).Tools(); got != nil {
		t.Fatalf("NewToolProvider(nil).Tools() = %v, want nil", got)
	}

	mod, err := New(&Deps{
		Workspace: t.TempDir(),
		Config:    Default(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	registrations := mod.Tools()
	if len(registrations) != 1 || registrations[0].Definition.Name != ToolName {
		t.Fatalf("Tools() = %+v, want 1 exec registration", registrations)
	}

	broker, err := toolbroker.New(&toolbroker.Options{
		Policy: approval.Policy{Version: "builtin-tools-v1", Rules: map[string]approval.Rule{}},
	})
	if err != nil {
		t.Fatalf("toolbroker.New: %v", err)
	}
	if err := broker.RegisterProvider(mod); err != nil {
		t.Fatalf("RegisterProvider: %v", err)
	}
	if _, ok := broker.Policy().Rules[ToolName]; !ok {
		t.Fatal("broker policy missing exec rule after RegisterProvider")
	}
}
