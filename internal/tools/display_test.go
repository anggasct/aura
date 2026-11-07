package tools_test

import (
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/tools"
	"github.com/anggasct/aura/internal/tools/displaytest"
)

var hiddenPattern = regexp.MustCompile(`\.{3}\[(\d+) bytes hidden\]`)

func requireCode(t *testing.T, err error, want tools.ErrorCode) {
	t.Helper()
	got, ok := tools.CodeOf(err)
	if !ok || got != want {
		t.Fatalf("error = %v, want code %s", err, want)
	}
}

func TestFieldDisplayScenarios(t *testing.T) {
	for _, scenario := range displaytest.Scenarios() {
		t.Run(scenario.Name, func(t *testing.T) {
			text := scenario.Text(t)
			if len(text) > tools.ApprovalDisplayLimit {
				t.Fatalf("display is %d bytes, limit %d", len(text), tools.ApprovalDisplayLimit)
			}
			scenario.Check(t, text, false)
		})
	}
}

func TestOnlyTheLastDeclaredStringTruncatesAndCountsWhatItHides(t *testing.T) {
	body := strings.Repeat("é", 3000)
	definition := displaytest.Definition([]string{"to", "subject", "body"})
	text, err := definition.ApprovalText(json.RawMessage(`{"body":"`+body+`","to":["a@mail.example"],"subject":"Hi"}`), nil)
	if err != nil {
		t.Fatalf("ApprovalText: %v", err)
	}
	if len(text) > tools.ApprovalDisplayLimit || !strings.HasPrefix(text, "to: [\"a@mail.example\"]\nsubject: Hi\nbody: ") {
		t.Fatalf("display = %q", text)
	}
	match := hiddenPattern.FindStringSubmatch(text)
	if match == nil {
		t.Fatalf("display has no hidden count: %q", text)
	}
	hidden, _ := strconv.Atoi(match[1])
	shown := strings.TrimSuffix(strings.TrimPrefix(text[strings.Index(text, "body: ")+len("body: "):], ""), match[0])
	if hidden+len(shown) != len(body) {
		t.Fatalf("hidden %d + shown %d bytes != body %d bytes", hidden, len(shown), len(body))
	}
	if strings.ContainsRune(shown, '\uFFFD') {
		t.Fatal("the cut split a multi byte character")
	}
}

func TestADisplayThatCannotFitFailsClosed(t *testing.T) {
	long := strings.Repeat("x", 1500)
	cases := []struct {
		name      string
		display   []string
		arguments string
	}{
		{"a long field before the last", []string{"body", "to"}, `{"body":"` + long + `","to":["a@mail.example"]}`},
		{"a long array never truncates", []string{"subject", "to"}, `{"subject":"s","to":[` + strings.Repeat(`"recipient@mail.example",`, 80) + `"z@mail.example"]}`},
		{"a long object never truncates", []string{"message"}, `{"message":{"text":"` + long + `"}}`},
		{"a long undeclared field", []string{"to"}, `{"to":"a@mail.example","note":"` + long + `"}`},
		{"a long field without display", nil, `{"body":"` + long + `"}`},
		{"a long string that is not last declared", []string{"body", "subject"}, `{"body":"` + long + `","subject":"s"}`},
		{"other fields leave no room for the marker", []string{"to", "body"}, `{"to":"` + strings.Repeat("r", 1190) + `","body":"` + long + `"}`},
		{"escaping grows the text past the limit", []string{"to", "subject"}, `{"subject":"s","to":"` + strings.Repeat(`\u202e`, 300) + `"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			text, err := displaytest.Definition(tc.display).ApprovalText(json.RawMessage(tc.arguments), nil)
			requireCode(t, err, tools.ErrorCodeDisplayTooLarge)
			if text != "" {
				t.Fatalf("a failed display returned text: %q", text)
			}
		})
	}
}

func TestNonPluginToolsKeepTheirRenderingWithEscapingAndTheSameLimit(t *testing.T) {
	definition := &tools.Definition{Name: "exec", Version: "v1", Effectful: true}
	text, err := definition.ApprovalText(json.RawMessage(`{ "command": ["echo", "a\u202eb"] }`), func(s string) string { return strings.ReplaceAll(s, "SECRET", "[REDACTED]") })
	if err != nil {
		t.Fatalf("ApprovalText: %v", err)
	}
	if text != `{"command":["echo","a\u202eb"]}` {
		t.Fatalf("display = %q", text)
	}
	raw := `{"command":["echo","` + string(rune(0x202e)) + `SECRET"]}`
	text, err = definition.ApprovalText(json.RawMessage(raw), func(s string) string { return strings.ReplaceAll(s, "SECRET", "[REDACTED]") })
	if err != nil || text != `{"command":["echo","\u{202e}[REDACTED]"]}` {
		t.Fatalf("display = %q, %v", text, err)
	}
	long, err := definition.ApprovalText(json.RawMessage(`{"command":["`+strings.Repeat("a", 5000)+`"]}`), nil)
	if err != nil || len(long) != tools.ApprovalDisplayLimit || !strings.HasSuffix(long, "...[truncated]") {
		t.Fatalf("long display = %d bytes, %v", len(long), err)
	}
}

func TestJSONDisplayShowsMarkupCharactersAsWritten(t *testing.T) {
	definition := &tools.Definition{Name: "exec", Version: "v1", Effectful: true}
	for name, tc := range map[string]struct{ raw, want string }{
		"escaped by the encoder": {raw: `{"cmd":"a 2\u003e\u00261 \u003cb"}`, want: `{"cmd":"a 2>&1 <b"}`},
		"upper case hex":         {raw: `{"cmd":"\u003C\u003E"}`, want: `{"cmd":"<>"}`},
		"already readable":       {raw: `{"cmd":"a > b"}`, want: `{"cmd":"a > b"}`},
		"literal backslash":      {raw: `{"p":"\\u003e"}`, want: `{"p":"\\u003e"}`},
		"backslash then escape":  {raw: `{"p":"\\\u003e"}`, want: `{"p":"\\>"}`},
		"quote stays escaped":    {raw: `{"p":"a\"b\n"}`, want: `{"p":"a\"b\n"}`},
		"other escape stays":     {raw: `{"p":"\u0041"}`, want: `{"p":"\u0041"}`},
	} {
		t.Run(name, func(t *testing.T) {
			text, err := definition.ApprovalText(json.RawMessage(tc.raw), nil)
			if err != nil || text != tc.want {
				t.Fatalf("display = %q, %v, want %q", text, err, tc.want)
			}
		})
	}
}

func TestJSONDisplayRedactsASecretHoldingMarkupCharacters(t *testing.T) {
	definition := &tools.Definition{Name: "exec", Version: "v1", Effectful: true}
	redact := func(s string) string { return strings.ReplaceAll(s, "p&ss<word>", "[REDACTED]") }
	text, err := definition.ApprovalText(json.RawMessage(`{"cmd":"login p\u0026ss\u003cword\u003e"}`), redact)
	if err != nil || text != `{"cmd":"login [REDACTED]"}` {
		t.Fatalf("display = %q, %v", text, err)
	}
}

func TestJSONDisplayKeepsTheLimitAfterReadableMarkup(t *testing.T) {
	definition := &tools.Definition{Name: "exec", Version: "v1", Effectful: true}
	raw := `{"cmd":"` + strings.Repeat(`\u0026`, 2000) + `"}`
	text, err := definition.ApprovalText(json.RawMessage(raw), nil)
	if err != nil || len(text) > tools.ApprovalDisplayLimit || !strings.HasSuffix(text, "...[truncated]") {
		t.Fatalf("display = %d bytes, %v", len(text), err)
	}
	if strings.Contains(text, `\u0026`) {
		t.Fatalf("display kept an escape: %q", text[:40])
	}
}

func TestRedactionHappensBeforeSizingAndTruncation(t *testing.T) {
	definition := displaytest.Definition([]string{"to", "body"})
	redact := func(s string) string { return strings.ReplaceAll(s, "hunter2", "[REDACTED]") }
	text, err := definition.ApprovalText(json.RawMessage(`{"to":"a@mail.example","body":"password hunter2 `+strings.Repeat("x", 3000)+`"}`), redact)
	if err != nil {
		t.Fatalf("ApprovalText: %v", err)
	}
	if strings.Contains(text, "hunter2") || !strings.Contains(text, "password [REDACTED]") {
		t.Fatalf("display = %q", text)
	}
}

func TestEmptyArgumentsRenderAsAnEmptyObject(t *testing.T) {
	for _, raw := range []string{``, `{}`} {
		text, err := displaytest.Definition([]string{"to"}).ApprovalText(json.RawMessage(raw), nil)
		if err != nil || text != "{}" {
			t.Fatalf("ApprovalText(%q) = %q, %v", raw, text, err)
		}
	}
	if _, err := displaytest.Definition(nil).ApprovalText(json.RawMessage(`[1]`), nil); err == nil {
		t.Fatal("a non object was rendered")
	}
}

func TestValidationRejectsDuplicateAndCaseVariantKeys(t *testing.T) {
	definition := &tools.Definition{Name: "mail", Version: "v1", Validator: func(raw json.RawMessage) (json.RawMessage, error) {
		if len(raw) == 0 {
			return json.RawMessage(`{}`), nil
		}
		if !json.Valid(raw) {
			return nil, errors.New("invalid json")
		}
		return raw, nil
	}}
	rejected := []struct{ name, raw string }{
		{"top level duplicate", `{"to":"a","to":"b"}`},
		{"top level case variant", `{"to":"a","TO":"b"}`},
		{"mixed case variant", `{"Subject":"a","sUBJECT":"b"}`},
		{"kelvin sign variant", `{"k":"a","` + string(rune(0x212A)) + `":"b"}`},
		{"nested duplicate", `{"a":{"b":1,"b":2}}`},
		{"duplicate inside an array element", `{"a":[{"x":1},{"y":1,"y":2}]}`},
		{"duplicate after a nested object", `{"a":{"b":1},"a":2}`},
	}
	for _, tc := range rejected {
		t.Run("rejects "+tc.name, func(t *testing.T) {
			_, err := definition.Validate(json.RawMessage(tc.raw))
			requireCode(t, err, tools.ErrorCodeArgumentsAmbiguous)
		})
	}
	accepted := []struct{ name, raw string }{
		{"empty", ``},
		{"distinct keys", `{"to":"a","cc":"b"}`},
		{"same key in sibling objects", `{"a":{"x":1},"b":{"x":2},"c":[{"x":1},{"x":2}]}`},
		{"nested case variants are data", `{"labels":{"A":1,"a":2}}`},
		{"scalar values", `{"to":"to","TO2":"TO"}`},
	}
	for _, tc := range accepted {
		t.Run("accepts "+tc.name, func(t *testing.T) {
			if _, err := definition.Validate(json.RawMessage(tc.raw)); err != nil {
				t.Fatalf("Validate: %v", err)
			}
		})
	}
}

func TestBuiltinToolsRejectDuplicateKeysToo(t *testing.T) {
	definition := tools.DefinitionsByKey()["read_file@v1"]
	for _, raw := range []string{`{"path":"/a","path":"/etc/passwd"}`, `{"path":"/a","PATH":"/b"}`} {
		if _, err := definition.Validate(json.RawMessage(raw)); err == nil {
			t.Errorf("Validate(%s) accepted ambiguous arguments", raw)
		}
	}
}

func TestDisplaySchemaCheck(t *testing.T) {
	strict := json.RawMessage(`{"type":"object","properties":{"to":{"type":"array"},"subject":{"type":"string"},"body":{"type":"string"}},"additionalProperties":false}`)
	cases := []struct {
		name    string
		display []string
		schema  string
		valid   bool
	}{
		{"no display needs no schema", nil, `{"type":"object"}`, true},
		{"declared fields on a strict schema", []string{"to", "subject", "body"}, string(strict), true},
		{"extra whitespace around false", []string{"to"}, `{"properties":{"to":{}},"additionalProperties" : false }`, true},
		{"field missing from properties", []string{"to", "bcc"}, string(strict), false},
		{"additional properties allowed", []string{"to"}, `{"properties":{"to":{}},"additionalProperties":true}`, false},
		{"additional properties unset", []string{"to"}, `{"properties":{"to":{}}}`, false},
		{"additional properties is a schema", []string{"to"}, `{"properties":{"to":{}},"additionalProperties":{"type":"string"}}`, false},
		{"no properties", []string{"to"}, `{"additionalProperties":false}`, false},
		{"default schema", []string{"to"}, `{"type":"object"}`, false},
		{"empty schema", []string{"to"}, ``, false},
		{"schema is not an object", []string{"to"}, `[]`, false},
		{"dotted display name", []string{"message.to"}, `{"properties":{"message.to":{}},"additionalProperties":false}`, false},
		{"duplicate display name", []string{"to", "to"}, string(strict), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tools.CheckDisplaySchema(tc.display, json.RawMessage(tc.schema))
			if tc.valid && err != nil {
				t.Fatalf("CheckDisplaySchema: %v", err)
			}
			if !tc.valid {
				requireCode(t, err, tools.ErrorCodeDisplayInvalid)
			}
		})
	}
}

func TestFieldApprovalAppliesToPluginEffectToolsOnly(t *testing.T) {
	plugin := &tools.PluginRef{ID: "mailer"}
	cases := []struct {
		name       string
		definition tools.Definition
		want       bool
	}{
		{"plugin effect", tools.Definition{Plugin: plugin, Effectful: true}, true},
		{"plugin read", tools.Definition{Plugin: plugin}, false},
		{"builtin effect", tools.Definition{Effectful: true}, false},
		{"mcp server tool", tools.Definition{Server: "docs", Effectful: true}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.definition.FieldApproval(); got != tc.want {
				t.Fatalf("FieldApproval = %v, want %v", got, tc.want)
			}
		})
	}
}
