package knowledge

import (
	"strings"
	"testing"
)

func runeText(codepoints ...rune) string {
	return string(codepoints)
}

func TestNormalizeRemovesInvisibleAndControlCharacters(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"zero width space", "ig" + runeText(0x200B) + "nore", "ignore"},
		{"zero width joiner and no-break", "a" + runeText(0x200D, 0x2060, 0xFEFF) + "b", "ab"},
		{"bidi overrides", "safe" + runeText(0x202E) + "evil" + runeText(0x202C), "safeevil"},
		{"bidi isolates and marks", runeText(0x2066, 0x200E, 0x2069, 0x061C) + "x", "x"},
		{"unicode tag characters", "a" + runeText(0xE0041, 0xE0001, 0xE007F) + "b", "ab"},
		{"soft hyphen", "co" + runeText(0xAD) + "operate", "cooperate"},
		{"c0 controls except newline and tab", "a" + runeText(0x00, 0x07, 0x1B) + "b\tc\nd", "ab\tc\nd"},
		{"crlf and lone carriage return", "a\r\nb\rc", "a\nb\nc"},
		{"next line and separators", "a" + runeText(0x85) + "b" + runeText(0x2028) + "c" + runeText(0x2029) + "d", "a\nb\nc\nd"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, format := range []Format{FormatMarkdown, FormatText} {
				if got := Normalize(tc.input, format); got != tc.want {
					t.Errorf("Normalize(%q, %s) = %q, want %q", tc.input, format, got, tc.want)
				}
			}
		})
	}
}

func TestNormalizeComposesToNFC(t *testing.T) {
	decomposed := "cafe" + runeText(0x301)
	if got, want := Normalize(decomposed, FormatText), "caf"+runeText(0xE9); got != want {
		t.Fatalf("Normalize = %q, want %q", got, want)
	}
	spaced := "cafe" + runeText(0x200B, 0x301)
	if got, want := Normalize(spaced, FormatText), "caf"+runeText(0xE9); got != want {
		t.Fatalf("Normalize with hidden separator = %q, want %q", got, want)
	}
}

func TestNormalizeDropsMarkdownCommentsOnlyForMarkdown(t *testing.T) {
	cases := []struct {
		name     string
		input    string
		markdown string
		text     string
	}{
		{"inline comment", "before<!-- hidden --> after", "before after", "before<!-- hidden --> after"},
		{"multiline comment", "a\n<!--\nsecret instruction\n-->\nb", "a\n\nb", "a\n<!--\nsecret instruction\n-->\nb"},
		{"unterminated comment drops the rest", "keep<!-- never closed\nmore", "keep", "keep<!-- never closed\nmore"},
		{"comment opener hidden by zero width", "a<!" + runeText(0x200B) + "-- x -->b", "ab", "a<!-- x -->b"},
		{"two comments", "<!--1-->x<!--2-->y", "xy", "<!--1-->x<!--2-->y"},
		{"no comment", "plain text", "plain text", "plain text"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Normalize(tc.input, FormatMarkdown); got != tc.markdown {
				t.Errorf("markdown = %q, want %q", got, tc.markdown)
			}
			if got := Normalize(tc.input, FormatText); got != tc.text {
				t.Errorf("text = %q, want %q", got, tc.text)
			}
		})
	}
}

func TestNormalizeIsLinearOnPathologicalComments(t *testing.T) {
	input := strings.Repeat("<!--", 200000)
	if got := Normalize(input, FormatMarkdown); got != "" {
		t.Fatalf("Normalize of repeated openers = %d bytes, want empty", len(got))
	}
	closers := strings.Repeat("-->", 200000)
	if got := Normalize(closers, FormatMarkdown); got != closers {
		t.Fatal("stray comment closers were altered")
	}
}
