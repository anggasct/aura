package knowledge

import (
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

type Format string

const (
	FormatMarkdown Format = "markdown"
	FormatText     Format = "text"
	FormatHTML     Format = "html"

	markdownCommentOpen  = "<!--"
	markdownCommentClose = "-->"
)

func Normalize(text string, format Format) string {
	text = unifyLineBreaks(text)
	text = stripInvisible(text)
	if format == FormatMarkdown {
		text = stripMarkdownComments(text)
	}
	return norm.NFC.String(text)
}

func unifyLineBreaks(text string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	return strings.Map(func(r rune) rune {
		switch r {
		case '\r', '\u0085', 0x2028, 0x2029:
			return '\n'
		}
		return r
	}, text)
}

func stripInvisible(text string) string {
	return strings.Map(func(r rune) rune {
		if invisibleRune(r) || unicode.IsControl(r) && r != '\n' && r != '\t' {
			return -1
		}
		return r
	}, text)
}

func invisibleRune(r rune) bool {
	switch {
	case r == 0x00AD, r == 0x061C, r == 0x180E, r == 0xFEFF:
		return true
	case r >= 0x200B && r <= 0x200F:
		return true
	case r >= 0x202A && r <= 0x202E:
		return true
	case r >= 0x2060 && r <= 0x2064:
		return true
	case r >= 0x2066 && r <= 0x206F:
		return true
	case r >= 0xFFF9 && r <= 0xFFFB:
		return true
	case r >= 0xE0000 && r <= 0xE007F:
		return true
	}
	return false
}

func stripMarkdownComments(text string) string {
	if !strings.Contains(text, markdownCommentOpen) {
		return text
	}
	var out strings.Builder
	out.Grow(len(text))
	for {
		open := strings.Index(text, markdownCommentOpen)
		if open < 0 {
			out.WriteString(text)
			return out.String()
		}
		out.WriteString(text[:open])
		rest := text[open+len(markdownCommentOpen):]
		closeAt := strings.Index(rest, markdownCommentClose)
		if closeAt < 0 {
			return out.String()
		}
		text = rest[closeAt+len(markdownCommentClose):]
	}
}
