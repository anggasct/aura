package fetch

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/toolbroker"
	"github.com/anggasct/aura/internal/platform/tools"
)

func TestToolDefinitionMatchesBuiltinCatalog(t *testing.T) {
	wantPtr, ok := tools.DefinitionsByKey()[ToolName+"@"+tools.Version]
	if !ok {
		t.Fatal("web_fetch@v1 missing from builtin catalog")
	}
	got := ToolDefinition()
	if got.Validator == nil {
		t.Fatal("ToolDefinition().Validator must not be nil")
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
		{name: "valid url", raw: `{"url":"https://example.com/doc"}`, ok: true},
		{name: "empty url", raw: `{"url":""}`, ok: false},
		{name: "missing url", raw: `{}`, ok: false},
		{name: "non-string url", raw: `{"url":123}`, ok: false},
		{name: "unknown key", raw: `{"url":"https://example.com","extra":true}`, ok: false},
		{name: "ambiguous key", raw: `{"url":"https://a.example","url":"https://b.example"}`, ok: false},
		{name: "trailing data", raw: `{"url":"https://example.com"} {}`, ok: false},
		{name: "not object", raw: `["https://example.com"]`, ok: false},
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
}

func TestToolRuleMatchesDefaultPolicy(t *testing.T) {
	want, ok := toolbroker.DefaultPolicy().Rules[ToolName]
	if !ok {
		t.Fatal("web_fetch missing from DefaultPolicy")
	}
	got := ToolRule()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ToolRule() = %+v, want %+v", got, want)
	}
}

func TestModuleConstructionAndRegistration(t *testing.T) {
	if _, err := New(nil); !fault.HasCode(err, ErrorCodeInvalidArgument) {
		t.Fatalf("New(nil) err = %v, want %s", err, ErrorCodeInvalidArgument)
	}
	invalid := Default()
	invalid.Timeout = 0
	if _, err := New(&Deps{Config: &invalid}); err == nil {
		t.Fatal("New accepted invalid config")
	}

	var nilMod *Module
	if len(nilMod.Tools()) != 0 || nilMod.Adapter() != nil {
		t.Fatal("nil Module returned non-empty tools or adapter")
	}

	mod, err := New(&Deps{})
	if err != nil {
		t.Fatalf("New(&Deps{}): %v", err)
	}
	var provider toolbroker.ToolProvider = mod
	registrations := provider.Tools()
	if len(registrations) != 1 || registrations[0].Definition.Name != ToolName || registrations[0].Adapter == nil {
		t.Fatalf("Tools() = %+v", registrations)
	}
	broker, err := toolbroker.New(&toolbroker.Options{})
	if err != nil {
		t.Fatalf("toolbroker.New: %v", err)
	}
	if err := broker.RegisterProvider(mod); err != nil {
		t.Fatalf("RegisterProvider: %v", err)
	}
	def, ok := broker.Definition(ToolName, tools.Version)
	if !ok || def.Validator == nil || !def.ExternalSink {
		t.Fatalf("registered definition = %+v, ok=%t", def, ok)
	}
}
