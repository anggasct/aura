package tools

import (
	"encoding/json"
	"testing"
)

func TestDefinitionValidationIsStrictAndCanonical(t *testing.T) {
	definition := &Definition{
		Name:    "read_file",
		Version: Version,
		Validator: func(raw json.RawMessage) (json.RawMessage, error) {
			value, err := DecodeObject(raw)
			if err != nil {
				return nil, err
			}
			if err := RejectUnknown(value, "path", "max_bytes"); err != nil {
				return nil, err
			}
			if err := ValidateString(value, "path"); err != nil {
				return nil, err
			}
			if err := ValidateOptionalInteger(value, "max_bytes", 1); err != nil {
				return nil, err
			}
			return json.Marshal(value)
		},
	}
	got, err := definition.Validate(json.RawMessage(`{"path":"/tmp/file"}`))
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if string(got) != `{"path":"/tmp/file"}` {
		t.Fatalf("canonical arguments = %s", got)
	}
	for _, raw := range []string{`{"path":"/tmp/file","extra":true}`, `{"path":1}`, `[]`} {
		if _, err := definition.Validate(json.RawMessage(raw)); err == nil {
			t.Errorf("Validate(%s) accepted invalid arguments", raw)
		}
	}
}

func TestDefinitionWithoutValidatorFailsClosed(t *testing.T) {
	definition := &Definition{Name: "unvalidated", Version: Version}
	if _, err := definition.Validate(json.RawMessage(`{}`)); err == nil {
		t.Fatal("Validate accepted arguments without a Validator")
	}
}

func TestListsToolMatchesExactNamesAndServerEntries(t *testing.T) {
	cases := map[string]struct {
		entries []string
		name    string
		server  string
		want    bool
	}{
		"exact built-in":             {[]string{"exec"}, "exec", "", true},
		"exact server tool":          {[]string{"mcp_docs_search"}, "mcp_docs_search", "docs", true},
		"server entry covers tool":   {[]string{"mcp_docs"}, "mcp_docs_search", "docs", true},
		"sibling tool":               {[]string{"mcp_docs_read"}, "mcp_docs_search", "docs", false},
		"longer exact entry":         {[]string{"mcp_docs_search_all"}, "mcp_docs_search", "docs", false},
		"other server entry":         {[]string{"mcp_files"}, "mcp_docs_search", "docs", false},
		"entry of a longer server":   {[]string{"mcp_docs_extra"}, "mcp_docs_search", "docs", false},
		"server entry on a built-in": {[]string{"mcp_"}, "exec", "", false},
		"empty entries":              {nil, "mcp_docs_search", "docs", false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := ListsTool(tc.entries, tc.name, tc.server); got != tc.want {
				t.Fatalf("ListsTool(%v, %q, %q) = %v, want %v", tc.entries, tc.name, tc.server, got, tc.want)
			}
		})
	}
}

func TestRunsAloneFollowsEffectsAndOrchestration(t *testing.T) {
	cases := []struct {
		name       string
		definition Definition
		want       bool
	}{
		{name: "read only", definition: Definition{Name: "read_file"}, want: false},
		{name: "effectful", definition: Definition{Name: "exec", Effectful: true}, want: true},
		{name: "orchestration", definition: Definition{Name: "handoff_to_owner", Orchestration: true}, want: true},
		{name: "both", definition: Definition{Name: "combo", Effectful: true, Orchestration: true}, want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.definition.RunsAlone(); got != tc.want {
				t.Errorf("%s RunsAlone() = %v, want %v", tc.definition.Name, got, tc.want)
			}
		})
	}
}
