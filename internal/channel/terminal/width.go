package terminal

import (
	"strings"

	"github.com/rivo/uniseg"
)

// stringWidth reports the display width of text for a monospace surface. It
// delegates to UAX #29 grapheme-aware width math so wide runes, combining
// marks, and emoji sequences measure as they render.
func stringWidth(text string) int {
	return uniseg.StringWidth(text)
}

// physicalLine is one rendered output line after wrapping.
type physicalLine struct {
	text  string
	width int
}

// wrapText splits text into physical lines at width columns, preserving hard
// newlines and breaking only at grapheme-cluster boundaries so a ZWJ emoji,
// flag, or modifier sequence is never cut across lines. A width below one
// disables wrapping; each logical line then stays whole so the caller can
// bound it by line count instead.
func wrapText(text string, width int) []physicalLine {
	var lines []physicalLine
	for _, logical := range strings.Split(text, "\n") {
		if width < 1 {
			lines = append(lines, physicalLine{text: logical, width: stringWidth(logical)})
			continue
		}
		var cur strings.Builder
		curWidth := 0
		clusters := uniseg.NewGraphemes(logical)
		for clusters.Next() {
			cluster := clusters.Str()
			cw := clusters.Width()
			if curWidth+cw > width && curWidth > 0 {
				lines = append(lines, physicalLine{text: cur.String(), width: curWidth})
				cur.Reset()
				curWidth = 0
			}
			cur.WriteString(cluster)
			curWidth += cw
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
