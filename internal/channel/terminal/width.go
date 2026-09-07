package terminal

import (
	"strings"

	"github.com/rivo/uniseg"
)

func stringWidth(text string) int {
	return uniseg.StringWidth(text)
}

type physicalLine struct {
	text  string
	width int
}

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

func truncateLines(lines []physicalLine, maxLines int) []physicalLine {
	if maxLines < 1 || len(lines) <= maxLines {
		return lines
	}
	kept := make([]physicalLine, 0, maxLines)
	kept = append(kept, physicalLine{text: "…", width: stringWidth("…")})
	kept = append(kept, lines[len(lines)-maxLines+1:]...)
	return kept
}
