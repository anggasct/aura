package tools

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBuiltinsExposeVersionedSchemas(t *testing.T) {
	definitions := Builtins()
	if len(definitions) != 14 {
		t.Fatalf("built-in definitions = %d, want 14", len(definitions))
	}
	for _, definition := range definitions {
		var schema map[string]any
		if err := json.Unmarshal(definition.Schema, &schema); err != nil {
			t.Fatalf("schema %s: %v", definition.Key(), err)
		}
		if schema["$schema"] != "https://json-schema.org/draft/2020-12/schema" {
			t.Errorf("schema %s has unexpected dialect", definition.Key())
		}
	}
}

func TestDefinitionValidationIsStrictAndCanonical(t *testing.T) {
	definition := DefinitionsByKey()["read_file@v1"]
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

func TestExecRequiresCommandArray(t *testing.T) {
	definition := DefinitionsByKey()["exec@v1"]
	_, err := definition.Validate(json.RawMessage(`{"command":[]}`))
	if err == nil || !strings.Contains(err.Error(), "non-empty") {
		t.Fatalf("Validate error = %v", err)
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

func TestEmailSendValidation(t *testing.T) {
	definition := DefinitionsByKey()[EmailSendToolName+"@"+Version]
	if !definition.Effectful || !definition.OptIn || ChannelOf(EmailSendToolName) != EmailChannel || ChannelOf("read_file") != "" {
		t.Fatalf("definition = %+v, want effectful, opt-in and bound to the email channel", definition)
	}
	cases := []struct {
		name    string
		raw     string
		want    string
		wantErr bool
	}{
		{name: "one plain address", raw: `{"to":"bob@example.net","subject":"Hi","body":"text"}`, want: `{"body":"text","subject":"Hi","to":"bob@example.net"}`},
		{name: "address is lowercased and trimmed", raw: `{"to":" Bob@Example.NET ","subject":"Hi","body":"text"}`, want: `{"body":"text","subject":"Hi","to":"bob@example.net"}`},
		{name: "display name form", raw: `{"to":"Bob <bob@example.net>","subject":"Hi","body":"text"}`, wantErr: true},
		{name: "two recipients", raw: `{"to":"a@example.net,b@example.net","subject":"Hi","body":"text"}`, wantErr: true},
		{name: "recipient list separated by space", raw: `{"to":"a@example.net b@example.net","subject":"Hi","body":"text"}`, wantErr: true},
		{name: "header injection in the recipient", raw: `{"to":"a@example.net\r\nBcc: x@evil.example","subject":"Hi","body":"text"}`, wantErr: true},
		{name: "not an address", raw: `{"to":"bob","subject":"Hi","body":"text"}`, wantErr: true},
		{name: "empty subject", raw: `{"to":"bob@example.net","subject":"","body":"text"}`, wantErr: true},
		{name: "empty body", raw: `{"to":"bob@example.net","subject":"Hi","body":""}`, wantErr: true},
		{name: "missing body", raw: `{"to":"bob@example.net","subject":"Hi"}`, wantErr: true},
		{name: "cc is not an argument", raw: `{"to":"bob@example.net","subject":"Hi","body":"text","cc":"c@example.net"}`, wantErr: true},
		{name: "subject at the bound", raw: `{"to":"bob@example.net","subject":"` + strings.Repeat("s", MaxEmailSubjectBytes) + `","body":"text"}`, want: `{"body":"text","subject":"` + strings.Repeat("s", MaxEmailSubjectBytes) + `","to":"bob@example.net"}`},
		{name: "subject over the bound", raw: `{"to":"bob@example.net","subject":"` + strings.Repeat("s", MaxEmailSubjectBytes+1) + `","body":"text"}`, wantErr: true},
		{name: "body over the bound", raw: `{"to":"bob@example.net","subject":"Hi","body":"` + strings.Repeat("b", MaxEmailBodyBytes+1) + `"}`, wantErr: true},
		{name: "non string recipient", raw: `{"to":["bob@example.net"],"subject":"Hi","body":"text"}`, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := definition.Validate(json.RawMessage(tc.raw))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Validate accepted %s", tc.raw)
				}
				return
			}
			if err != nil || string(got) != tc.want {
				t.Fatalf("Validate = %s, %v; want %s", got, err, tc.want)
			}
		})
	}
}
