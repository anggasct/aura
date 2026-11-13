package displaytest

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/platform/tools"
)

type Scenario struct {
	Name       string
	Display    []string
	Arguments  string
	Want       []string
	InOrder    []string
	WantAbsent []string
}

func Definition(display []string) *tools.Definition {
	return &tools.Definition{
		Name:      "plugin_mailer_send",
		Version:   "v1",
		Plugin:    &tools.PluginRef{ID: "mailer", Description: "Send mail."},
		Effectful: true,
		Display:   display,
	}
}

func (s *Scenario) Text(t testing.TB) string {
	t.Helper()
	text, err := Definition(s.Display).ApprovalText(json.RawMessage(s.Arguments), nil)
	if err != nil {
		t.Fatalf("scenario %q: ApprovalText: %v", s.Name, err)
	}
	return text
}

func recipients(count int) string {
	addresses := make([]string, 0, count)
	for i := range count {
		addresses = append(addresses, `"recipient-`+string(rune('a'+i))+`@mail.example"`)
	}
	return "[" + strings.Join(addresses, ",") + "]"
}

func Scenarios() []Scenario {
	mail := []string{"to", "cc", "subject", "body"}
	return []Scenario{
		{
			Name:      "a planted argument order cannot hide the recipients",
			Display:   mail,
			Arguments: `{"body":"` + strings.Repeat("filler ", 400) + `","subject":"Quarterly numbers","cc":["audit@mail.example"],"to":["alice@mail.example","bob@mail.example"]}`,
			Want:      []string{`"alice@mail.example"`, `"bob@mail.example"`, `"audit@mail.example"`, "Quarterly numbers", "bytes hidden]"},
			InOrder:   []string{"to: ", "cc: ", "subject: ", "body: ", "bytes hidden]"},
		},
		{
			Name:      "an oversized body is the only thing that truncates",
			Display:   mail,
			Arguments: `{"to":["alice@mail.example"],"subject":"Hi","body":"` + strings.Repeat("x", 6000) + `"}`,
			Want:      []string{`"alice@mail.example"`, "subject: Hi", "bytes hidden]"},
		},
		{
			Name:      "a to placed after a long field still shows every recipient",
			Display:   []string{"subject", "to"},
			Arguments: `{"to":` + recipients(20) + `,"subject":"` + strings.Repeat("s", 200) + `"}`,
			Want:      []string{`"recipient-a@mail.example"`, `"recipient-t@mail.example"`, strings.Repeat("s", 200)},
			InOrder:   []string{"subject: ", "to: "},
		},
		{
			Name:      "undeclared fields are complete and come before the truncating field",
			Display:   mail,
			Arguments: `{"to":["alice@mail.example"],"subject":"Hi","body":"` + strings.Repeat("y", 5000) + `","reply_to_id":"msg-17","track":{"opens":true,"links":["https://t.example/a"]}}`,
			Want:      []string{`"alice@mail.example"`, "reply_to_id: msg-17", `track: {"opens":true,"links":["https://t.example/a"]}`, "bytes hidden]"},
			InOrder:   []string{"to: ", "reply_to_id: ", "track: ", "body: "},
		},
		{
			Name:       "a tool without display hides nothing",
			Arguments:  `{"to":["alice@mail.example"],"subject":"Hi","body":"short text","cc":["bob@mail.example"]}`,
			Want:       []string{`"alice@mail.example"`, `"bob@mail.example"`, "subject: Hi", "body: short text"},
			InOrder:    []string{"body: ", "cc: ", "subject: ", "to: "},
			WantAbsent: []string{"hidden"},
		},
		{
			Name:       "a display naming a nested object shows it whole",
			Display:    []string{"message", "note"},
			Arguments:  `{"note":"n","message":{"to":["alice@mail.example"],"cc":["bob@mail.example"],"headers":{"x-a":"1"}}}`,
			Want:       []string{`message: {"to":["alice@mail.example"],"cc":["bob@mail.example"],"headers":{"x-a":"1"}}`, "note: n"},
			WantAbsent: []string{"hidden"},
		},
		{
			Name:       "control and direction characters are escaped",
			Display:    mail,
			Arguments:  "{\"to\":[\"alice@mail.example\\u202e\"],\"subject\":\"line one\\nto: attacker@evil.example\",\"body\":\"c1\\u0085 rlo\\u202e bidi\\u2066 esc\\u001b[2J nul\\u0000\"}",
			Want:       []string{`\u{202e}`, `\n`, `\u{0085}`, `\u{2066}`, `\u{001b}`, `\u{0000}`},
			WantAbsent: []string{"\u202e", "\u0085", "\u2066", "\u001b", "\x00"},
		},
		{
			Name:       "invisible and stacked runes are escaped",
			Display:    mail,
			Arguments:  `{"to":["alice@mail.example"],"subject":"s` + "\U000E0041\u200b\ufe0f" + ` a` + strings.Repeat("\u0301", 5) + `","body":"C:\\tmp"}`,
			Want:       []string{`\u{e0041}`, `\u{200b}`, `\u{fe0f}`, `\u{0301}`, `C:\\tmp`},
			WantAbsent: []string{"\U000E0041", "\ufe0f", strings.Repeat("\u0301", 3)},
		},
		{
			Name:      "backticks and links stay text",
			Display:   mail,
			Arguments: `{"to":["alice@mail.example"],"subject":"Hi","body":"x` + "```" + `[ok](https://evil)"}`,
			Want:      []string{"[ok](https://evil)"},
		},
		{
			Name:       "the largest display that fits is complete",
			Display:    mail,
			Arguments:  `{"to":` + recipients(20) + `,"cc":[],"subject":"` + strings.Repeat("t", 160) + `","body":"short"}`,
			Want:       []string{`"recipient-t@mail.example"`, "body: short"},
			WantAbsent: []string{"hidden"},
		},
	}
}

func squash(text string) string {
	return strings.Join(strings.Fields(text), "")
}

func (s *Scenario) Check(t testing.TB, card string, ignoreWhitespace bool) {
	t.Helper()
	prepare := func(text string) string { return text }
	if ignoreWhitespace {
		prepare = squash
	}
	rendered := prepare(card)
	for _, want := range s.Want {
		if !strings.Contains(rendered, prepare(want)) {
			t.Errorf("scenario %q: card lacks %q:\n%s", s.Name, want, card)
		}
	}
	for _, absent := range s.WantAbsent {
		if strings.Contains(card, absent) {
			t.Errorf("scenario %q: card carries %q:\n%s", s.Name, absent, card)
		}
	}
	cursor := 0
	for _, marker := range s.InOrder {
		index := strings.Index(rendered[cursor:], prepare(marker))
		if index < 0 {
			t.Errorf("scenario %q: card lacks %q after offset %d:\n%s", s.Name, marker, cursor, card)
			return
		}
		cursor += index + len(prepare(marker))
	}
}
