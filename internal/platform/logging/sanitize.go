package logging

import (
	"strings"
	"unicode/utf8"
)

const maxSanitizedBytes = 1 << 20

func SanitizeText(text string) string {
	text = strings.ToValidUTF8(text, "\uFFFD")
	var out strings.Builder
	for i := 0; i < len(text); {
		r, size := utf8.DecodeRuneInString(text[i:])
		switch {
		case r == '\x1b':
			i = skipEscape(text, i+size)
		case r == '\x9b':
			i = skipCSI(text, i+size)
		case r == '\x9d':
			i = skipOSC(text, i+size)
		case r == '\n':
			out.WriteRune(r)
			i += size
		case r < 0x20 || (r >= 0x7f && r <= 0x9f):
			i += size
		default:
			out.WriteRune(r)
			i += size
		}
	}
	return limitSanitizedText(out.String())
}

func limitSanitizedText(text string) string {
	if len(text) <= maxSanitizedBytes {
		return text
	}
	cut := maxSanitizedBytes
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut]
}

func skipEscape(text string, i int) int {
	if i >= len(text) {
		return i
	}
	switch text[i] {
	case '[':
		return skipCSI(text, i+1)
	case ']':
		return skipOSC(text, i+1)
	default:
		for i < len(text) {
			b := text[i]
			i++
			if b >= 0x30 && b <= 0x7e {
				return i
			}
		}
	}
	return i
}

func skipCSI(text string, i int) int {
	for i < len(text) {
		b := text[i]
		i++
		if b >= 0x40 && b <= 0x7e {
			return i
		}
	}
	return i
}

func skipOSC(text string, i int) int {
	for i < len(text) {
		if text[i] == '\a' {
			return i + 1
		}
		if text[i] == '\x1b' && i+1 < len(text) && text[i+1] == '\\' {
			return i + 2
		}
		i++
	}
	return i
}
