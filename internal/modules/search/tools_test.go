package search

import (
	"encoding/json"
	"reflect"
	"slices"
	"testing"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/secret/secrettest"
	"github.com/anggasct/aura/internal/platform/toolbroker"
	"github.com/anggasct/aura/internal/platform/tools"
)

func TestToolDefinitionMatchesBuiltinCatalog(t *testing.T) {
	got := ToolDefinition()
	if got.Name != ToolName || got.Version != tools.Version || got.Validator == nil {
		t.Fatalf("ToolDefinition() = %+v", got)
	}
	if !got.ExternalSink || got.RequiresApproval || got.Effectful || !slices.Equal(got.RequiredCapabilities, []string{"provider-search"}) {
		t.Fatalf("ToolDefinition() flags = %+v", got)
	}

	payloads := []struct {
		name string
		raw  string
		ok   bool
	}{
		{name: "minimal query", raw: `{"query":"golang"}`, ok: true},
		{name: "query with max_results", raw: `{"query":"golang","max_results":5}`, ok: true},
		{name: "empty query", raw: `{"query":""}`, ok: false},
		{name: "missing query", raw: `{"max_results":5}`, ok: false},
		{name: "non-string query", raw: `{"query":123}`, ok: false},
		{name: "max_results zero", raw: `{"query":"golang","max_results":0}`, ok: false},
		{name: "max_results negative", raw: `{"query":"golang","max_results":-1}`, ok: false},
		{name: "max_results float", raw: `{"query":"golang","max_results":1.5}`, ok: false},
		{name: "max_results string", raw: `{"query":"golang","max_results":"5"}`, ok: false},
		{name: "unknown key", raw: `{"query":"golang","extra":true}`, ok: false},
		{name: "ambiguous key", raw: `{"query":"a","query":"b"}`, ok: false},
		{name: "trailing data", raw: `{"query":"golang"} {}`, ok: false},
		{name: "not object", raw: `["golang"]`, ok: false},
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
}

func TestToolRuleMatchesDefaultPolicy(t *testing.T) {
	def := ToolDefinition()
	want := toolbroker.DefaultRule(&def)
	got := ToolRule()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ToolRule() = %+v, want %+v", got, want)
	}
}

func TestModuleConstructionAndRegistration(t *testing.T) {
	if _, err := New(nil); !fault.HasCode(err, ErrorCodeInvalidArgument) {
		t.Fatalf("New(nil) err = %v, want %s", err, ErrorCodeInvalidArgument)
	}
	badEndpoint := Config{Provider: ProviderSearXNG, Endpoint: "http://localhost:8888/search", MaxResults: 5}
	if _, err := New(&Deps{Config: &badEndpoint}); err == nil {
		t.Fatal("New accepted invalid endpoint")
	}

	var nilMod *Module
	if len(nilMod.Tools()) != 0 || nilMod.Adapter() != nil || len(nilMod.Secrets()) != 0 {
		t.Fatal("nil Module returned non-empty state")
	}

	t.Setenv("AURA_TEST_SEARCH_MOD_KEY", "mod-secret-value")
	cfg := Config{Provider: ProviderBrave, CredentialRef: secrettest.Ref("env://AURA_TEST_SEARCH_MOD_KEY"), MaxResults: 5}
	mod, err := New(&Deps{Config: &cfg})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := mod.Secrets(); !slices.Equal(got, []string{"mod-secret-value"}) {
		t.Fatalf("Secrets() = %v, want [mod-secret-value]", got)
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
