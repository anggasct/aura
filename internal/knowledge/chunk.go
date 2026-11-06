package knowledge

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"unicode/utf8"
)

const (
	MaxChunkRunes     = 1500
	ChunkOverlapRunes = 150
	MaxHeadingRunes   = 120

	chunkBreakWindow = 300
	maxHeadingLevel  = 6
	maxHeadingIndent = 3
)

type Chunk struct {
	Ordinal int
	Heading string
	Content string
	SHA256  string
}

func SplitChunks(text string, limit int) (chunks []Chunk, exceeded bool) {
	heading := ""
	var body []string
	flush := func() bool {
		for _, piece := range splitBody(strings.Join(body, "\n")) {
			if len(chunks) >= limit {
				return false
			}
			chunks = append(chunks, Chunk{Ordinal: len(chunks), Heading: heading, Content: piece, SHA256: chunkDigest(heading, piece)})
		}
		body = body[:0]
		return true
	}
	fence := ""
	for _, line := range strings.Split(text, "\n") {
		if marker := fenceMarker(line); marker != "" {
			switch {
			case fence == "":
				fence = marker
			case strings.HasPrefix(marker, fence):
				fence = ""
			}
		}
		if title, ok := headingTitle(line); ok && fence == "" {
			if !flush() {
				return chunks, true
			}
			heading = title
			continue
		}
		body = append(body, line)
	}
	if !flush() {
		return chunks, true
	}
	return chunks, false
}

func chunkDigest(heading, content string) string {
	sum := sha256.Sum256([]byte(heading + "\x00" + content))
	return hex.EncodeToString(sum[:])
}

func fenceMarker(line string) string {
	trimmed := strings.TrimLeft(line, " ")
	if len(line)-len(trimmed) > maxHeadingIndent {
		return ""
	}
	for _, marker := range []string{"```", "~~~"} {
		if strings.HasPrefix(trimmed, marker) {
			return marker
		}
	}
	return ""
}

func headingTitle(line string) (string, bool) {
	trimmed := strings.TrimLeft(line, " ")
	if len(line)-len(trimmed) > maxHeadingIndent {
		return "", false
	}
	level := 0
	for level < len(trimmed) && trimmed[level] == '#' {
		level++
	}
	if level == 0 || level > maxHeadingLevel {
		return "", false
	}
	rest := trimmed[level:]
	if rest != "" && rest[0] != ' ' && rest[0] != '\t' {
		return "", false
	}
	title := strings.TrimRight(strings.TrimSpace(rest), "#")
	title = strings.Join(strings.Fields(title), " ")
	return truncateRunes(title, MaxHeadingRunes), true
}

func truncateRunes(text string, limit int) string {
	if utf8.RuneCountInString(text) <= limit {
		return text
	}
	return strings.TrimSpace(string([]rune(text)[:limit]))
}

func splitBody(body string) []string {
	runes := []rune(strings.TrimSpace(body))
	var pieces []string
	for start := 0; start < len(runes); {
		end := min(start+MaxChunkRunes, len(runes))
		if end < len(runes) {
			end = breakPoint(runes, start, end)
		}
		if piece := strings.TrimSpace(string(runes[start:end])); piece != "" {
			pieces = append(pieces, piece)
		}
		if end >= len(runes) {
			break
		}
		start = end - ChunkOverlapRunes
	}
	return pieces
}

func breakPoint(runes []rune, start, end int) int {
	floor := max(start+MaxChunkRunes-chunkBreakWindow, start+1)
	for _, separator := range []rune{'\n', ' '} {
		for i := end - 1; i >= floor; i-- {
			if runes[i] == separator {
				return i + 1
			}
		}
	}
	return end
}
