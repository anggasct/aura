package terminal

import (
	"strings"
	"unicode"
)

// wideRanges approximates East Asian Wide and Fullwidth plus common emoji
// blocks for display-width math. Widths are presentation-only; an unknown
// rune degrades to width one, never to a runtime failure.
var wideRanges = [][2]rune{
	{0x1100, 0x115F}, {0x2329, 0x232A}, {0x2E80, 0x303E}, {0x3041, 0x33FF},
	{0x3400, 0x4DBF}, {0x4E00, 0x9FFF}, {0xA000, 0xA4CF}, {0xA960, 0xA97F},
	{0xAC00, 0xD7A3}, {0xF900, 0xFAFF}, {0xFE10, 0xFE19}, {0xFE30, 0xFE6F},
	{0xFF00, 0xFF60}, {0xFFE0, 0xFFE6}, {0x1F300, 0x1F64F}, {0x1F900, 0x1F9FF},
	{0x20000, 0x2FFFD}, {0x30000, 0x3FFFD},
}

// runeWidth returns the display width of one rune: zero for combining marks,
// format controls, and controls; two for wide runes; one otherwise.
func runeWidth(r rune) int {
	switch {
	case r == 0:
		return 0
	case unicode.Is(unicode.Mn, r), unicode.Is(unicode.Me, r), unicode.Is(unicode.Cf, r):
		return 0
	case r < 0x20 || (r >= 0x7f && r < 0xa0):
		return 0
	}
	for _, rg := range wideRanges {
		if r >= rg[0] && r <= rg[1] {
			return 2
		}
	}
	return 1
}

// stringWidth sums rune display widths.
func stringWidth(text string) int {
	total := 0
	for _, r := range text {
		total += runeWidth(r)
	}
	return total
}

// physicalLine is one rendered output line after wrapping.
type physicalLine struct {
	text  string
	width int
}

// wrapText splits text into physical lines at width columns, preserving hard
// newlines. A width below one disables wrapping; each logical line then stays
// whole so the caller can bound it by line count instead.
func wrapText(text string, width int) []physicalLine {
	var lines []physicalLine
	for _, logical := range strings.Split(text, "\n") {
		if width < 1 {
			lines = append(lines, physicalLine{text: logical, width: stringWidth(logical)})
			continue
		}
		var cur strings.Builder
		curWidth := 0
		for _, r := range logical {
			rw := runeWidth(r)
			if curWidth+rw > width && curWidth > 0 {
				lines = append(lines, physicalLine{text: cur.String(), width: curWidth})
				cur.Reset()
				curWidth = 0
			}
			cur.WriteRune(r)
			curWidth += rw
		}
		lines = append(lines, physicalLine{text: cur.String(), width: curWidth})
	}
	return lines
}

// truncateLines keeps at most maxLines physical lines, dropping from the top
// and marking the cut with an ellipsis line so recent output stays visible on
// tiny terminals while memory and repaint stay bounded.
func truncateLines(lines []physicalLine, maxLines int) []physicalLine {
	if maxLines < 1 || len(lines) <= maxLines {
		return lines
	}
	kept := make([]physicalLine, 0, maxLines)
	kept = append(kept, physicalLine{text: "…", width: stringWidth("…")})
	kept = append(kept, lines[len(lines)-maxLines+1:]...)
	return kept
}
