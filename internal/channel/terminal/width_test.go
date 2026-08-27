package terminal

import (
	"strings"
	"testing"
)

func TestRuneWidth(t *testing.T) {
	cases := []struct {
		name string
		text string
		want int
	}{
		{"ascii", "abc", 3},
		{"cjk wide", "你好", 4},
		{"combining accent", "e\u0301", 1},
		{"zero-width joiner", "a\u200db", 2},
		{"emoji wide", "\U0001F600", 2},
		{"fullwidth latin", "Ａ", 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := stringWidth(tc.text); got != tc.want {
				t.Errorf("stringWidth(%q) = %d, want %d", tc.text, got, tc.want)
			}
		})
	}
}

func TestWrapTextSplitsAtWidth(t *testing.T) {
	lines := wrapText("abcdefghij", 4)
	if len(lines) != 3 {
		t.Fatalf("lines = %d, want 3: %+v", len(lines), lines)
	}
	if lines[0].text != "abcd" || lines[1].text != "efgh" || lines[2].text != "ij" {
		t.Errorf("wrapped = %+v", lines)
	}
	for _, line := range lines {
		if line.width > 4 {
			t.Errorf("line %q width %d exceeds 4", line.text, line.width)
		}
	}
}

func TestWrapTextKeepsWideRunesWhole(t *testing.T) {
	lines := wrapText("你a你", 2)
	// 你 (2) fills a line; a (1) cannot fit the next 你 (2) on the same
	// two-column line, so each line stays within the width bound.
	var got []string
	for _, line := range lines {
		got = append(got, line.text)
	}
	if strings.Join(got, "|") != "你|a|你" {
		t.Errorf("wrapped = %q", strings.Join(got, "|"))
	}
}

func TestWrapTextPreservesHardNewlines(t *testing.T) {
	lines := wrapText("ab\ncd", 10)
	if len(lines) != 2 || lines[0].text != "ab" || lines[1].text != "cd" {
		t.Errorf("wrapped = %+v", lines)
	}
}

func TestTruncateLinesBounded(t *testing.T) {
	var lines []physicalLine
	for i := range maxDisplayLines * 3 {
		lines = append(lines, physicalLine{text: string(rune('a' + i%26)), width: 1})
	}
	kept := truncateLines(lines, maxDisplayLines)
	if len(kept) != maxDisplayLines {
		t.Fatalf("kept = %d, want %d", len(kept), maxDisplayLines)
	}
	if kept[0].text != "…" {
		t.Errorf("first kept = %q, want ellipsis marker", kept[0].text)
	}
}

func TestWrapTextKeepsGraphemeClustersWhole(t *testing.T) {
	cases := []struct {
		name  string
		text  string
		width int
		want  []string
	}{
		// ZWJ sequence: one cluster, display width two.
		{"zwj emoji", "\U0001F469\u200D\U0001F4BB\U0001F469\u200D\U0001F4BB", 2, []string{"\U0001F469\u200D\U0001F4BB", "\U0001F469\u200D\U0001F4BB"}},
		// Regional indicator pair: one flag cluster, width two.
		{"flag", "\U0001F1FA\U0001F1F8\U0001F1FA\U0001F1F8", 2, []string{"\U0001F1FA\U0001F1F8", "\U0001F1FA\U0001F1F8"}},
		// Skin-tone modifier: one cluster, width two.
		{"modifier", "\U0001F44D\U0001F3FD\U0001F44D\U0001F3FD", 2, []string{"\U0001F44D\U0001F3FD", "\U0001F44D\U0001F3FD"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lines := wrapText(tc.text, tc.width)
			got := make([]string, 0, len(lines))
			for _, line := range lines {
				got = append(got, line.text)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("wrapped %q into %d lines (%q), want %d (%q)", tc.text, len(got), got, len(tc.want), tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("line %d = %q, want %q (cluster split?)", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestStringWidthGraphemeAware(t *testing.T) {
	cases := []struct {
		name string
		text string
		want int
	}{
		{"zwj technologist", "\U0001F469\u200D\U0001F4BB", 2},
		{"flag", "\U0001F1FA\U0001F1F8", 2},
		{"skin tone modifier", "\U0001F44D\U0001F3FD", 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := stringWidth(tc.text); got != tc.want {
				t.Errorf("stringWidth(%q) = %d, want %d", tc.text, got, tc.want)
			}
		})
	}
}
