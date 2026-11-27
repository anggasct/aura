package logging

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSanitizeTextStripsControlAndEscapeSequences(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "CSI color and clear",
			input: "hello\x1b[31m red\x1b[0m\x1b[2J",
			want:  "hello red",
		},
		{
			name:  "8-bit CSI",
			input: "before\u009b31mred\u009b0mafter",
			want:  "beforeredafter",
		},
		{
			name:  "OSC terminated by BEL",
			input: "open\x1b]8;;https://example.test\alink\x1b]8;;\adone",
			want:  "openlinkdone",
		},
		{
			name:  "OSC terminated by ST",
			input: "head\x1b]0;window title\x1b\\tail",
			want:  "headtail",
		},
		{
			name:  "8-bit OSC terminated by BEL",
			input: "a\u009d0;title\ab",
			want:  "ab",
		},
		{
			name:  "plain ESC sequence",
			input: "a\x1b(Bb",
			want:  "ab",
		},
		{
			name:  "C0 and C1 controls preserve newline",
			input: "line1\x00\x07\x08\r\nline2\x7f\u0085end",
			want:  "line1\nline2end",
		},
		{
			name:  "invalid UTF-8 replaced",
			input: "ok\xff\xfe!",
			want:  "ok\uFFFD!",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := SanitizeText(tc.input); got != tc.want {
				t.Errorf("SanitizeText(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestSanitizeTextTruncatesAtRuneBoundary(t *testing.T) {
	ascii := strings.Repeat("a", maxSanitizedBytes+128)
	gotASCII := SanitizeText(ascii)
	if len(gotASCII) != maxSanitizedBytes {
		t.Fatalf("len(SanitizeText(ascii)) = %d, want %d", len(gotASCII), maxSanitizedBytes)
	}

	prefix := strings.Repeat("a", maxSanitizedBytes-1)
	multiByte := prefix + "é" + "tail"
	gotMulti := SanitizeText(multiByte)
	if len(gotMulti) != maxSanitizedBytes-1 {
		t.Fatalf("len(SanitizeText(multiByte)) = %d, want %d", len(gotMulti), maxSanitizedBytes-1)
	}
	if !utf8.ValidString(gotMulti) {
		t.Fatal("truncated output is not valid UTF-8")
	}
}
