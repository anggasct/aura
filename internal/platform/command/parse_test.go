package command

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestParseRecognizesOnlyLeadingCommandTokens(t *testing.T) {
	tests := []struct {
		text string
		ok   bool
		name string
		args []string
	}{
		{"/help", true, "help", nil},
		{"/HELP", true, "help", nil},
		{"  /Skill-Create  n  a  b ", true, "skill-create", []string{"n", "a", "b"}},
		{"/session sess_1", true, "session", []string{"sess_1"}},
		{"/a" + strings.Repeat("b", 31), true, "a" + strings.Repeat("b", 31), nil},
		{"/a" + strings.Repeat("b", 32), false, "", nil},
		{"/", false, "", nil},
		{"/1abc", false, "", nil},
		{"/-abc", false, "", nil},
		{"/a_b", false, "", nil},
		{"/hélp", false, "", nil},
		{"/Kill", false, "", nil},
		{"//help", false, "", nil},
		{"hello /help", false, "", nil},
		{"<@123> /help", false, "", nil},
		{"", false, "", nil},
		{"   ", false, "", nil},
		{".", false, "", nil},
	}
	for _, tt := range tests {
		t.Run(tt.text, func(t *testing.T) {
			inv, ok := Parse(tt.text)
			if ok != tt.ok {
				t.Fatalf("Parse(%q) ok = %v, want %v", tt.text, ok, tt.ok)
			}
			if !ok {
				return
			}
			if inv.Name != tt.name || !slices.Equal(inv.Args, tt.args) {
				t.Errorf("Parse(%q) = %+v, want %s %v", tt.text, inv, tt.name, tt.args)
			}
			if inv.Token != strings.Fields(tt.text)[0] {
				t.Errorf("token = %q", inv.Token)
			}
		})
	}
}

func TestTokenizeSplitsQuotedArguments(t *testing.T) {
	tests := []struct {
		name string
		text string
		want []string
	}{
		{"empty", "", nil},
		{"only spaces", "  \t ", nil},
		{"plain words", "add  nightly\tdaily", []string{"add", "nightly", "daily"}},
		{"double quotes group", `add "two words" tail`, []string{"add", "two words", "tail"}},
		{"single quotes group", `add 'two words' tail`, []string{"add", "two words", "tail"}},
		{"quotes inside a flag value", `--name="a b" --x='c d'`, []string{"--name=a b", "--x=c d"}},
		{"adjacent quoted and bare text join", `pre"fix post"suf`, []string{"prefix postsuf"}},
		{"empty quoted token", `a "" b`, []string{"a", "", "b"}},
		{"the other quote is literal inside quotes", `"it's" 'say "hi"'`, []string{"it's", `say "hi"`}},
		{"backslash escapes a quote", `"say \"hi\"" it\'s`, []string{`say "hi"`, "it's"}},
		{"backslash escapes a space", `one\ token two`, []string{"one token", "two"}},
		{"backslash escapes a backslash", `a\\b`, []string{`a\b`}},
		{"no expansion", `$HOME * ~ $(x) ` + "`y`", []string{"$HOME", "*", "~", "$(x)", "`y`"}},
		{"multi-byte text survives", `"héllo wörld" ✓`, []string{"héllo wörld", "✓"}},
		{"newline separates", "a\nb", []string{"a", "b"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Tokenize(tt.text)
			if err != nil || !slices.Equal(got, tt.want) {
				t.Fatalf("Tokenize(%q) = %q, %v; want %q", tt.text, got, err, tt.want)
			}
		})
	}
}

func TestTokenizeRefusesWhatItCannotReadWithinItsBounds(t *testing.T) {
	tests := []struct {
		name string
		text string
		want error
	}{
		{"open double quote", `a "b`, errTokenizeOpenQuote},
		{"open single quote", `a 'b`, errTokenizeOpenQuote},
		{"dangling backslash", `a b\`, errTokenizeDanglingEscape},
		{"too long", strings.Repeat("a", MaxCommandBytes+1), errTokenizeTooLong},
		{"too many tokens", strings.Repeat("a ", MaxTokens+1), errTokenizeTooMany},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Tokenize(tt.text)
			if got != nil || !errors.Is(err, tt.want) || !IsUsageError(err) {
				t.Fatalf("Tokenize() = %q, %v; want a usage error wrapping %v", got, err, tt.want)
			}
		})
	}
	t.Run("the bounds themselves are accepted", func(t *testing.T) {
		if got, err := Tokenize(strings.Repeat("a ", MaxTokens)); err != nil || len(got) != MaxTokens {
			t.Fatalf("Tokenize(max tokens) = %d tokens, %v", len(got), err)
		}
		if got, err := Tokenize(strings.Repeat("a", MaxCommandBytes)); err != nil || len(got) != 1 {
			t.Fatalf("Tokenize(max bytes) = %d tokens, %v", len(got), err)
		}
	})
}
