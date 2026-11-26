package shellexec

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/toolbroker"
	"github.com/anggasct/aura/internal/platform/tools"
)

func TestShellexecToolMatchesBuiltinCatalog(t *testing.T) {
	got := ToolDefinition()
	if got.Name != ToolName || got.Version != tools.Version || got.Validator == nil {
		t.Fatalf("ToolDefinition() = %+v", got)
	}
	if !got.RequiresApproval || !got.Effectful || !slices.Equal(got.RequiredCapabilities, []string{"exec-linux"}) {
		t.Fatalf("ToolDefinition() = %+v, want RequiresApproval, Effectful, and exec-linux", got)
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
			if (gotErr == nil) != tc.ok {
				t.Fatalf("got.Validate(%s) err = %v, want ok=%t", tc.raw, gotErr, tc.ok)
			}
			if tc.ok && !json.Valid(gotOut) {
				t.Fatalf("validated output is not valid JSON: %s", gotOut)
			}
		})
	}

	wantRule := toolbroker.DefaultRule(&got)
	if gotRule := ToolRule(); !reflect.DeepEqual(gotRule, wantRule) {
		t.Fatalf("ToolRule() = %+v, want %+v", gotRule, wantRule)
	}
}

func TestExecRequiresCommandArray(t *testing.T) {
	definition := ToolDefinition()
	_, err := definition.Validate(json.RawMessage(`{"command":[]}`))
	if err == nil || !strings.Contains(err.Error(), "non-empty") {
		t.Fatalf("Validate error = %v", err)
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
