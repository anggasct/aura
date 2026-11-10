package connector

import (
	"mime"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/net/html/charset"
)

const replacement = '�'

func unwanted(r rune) bool {
	return unicode.IsControl(r) || unicode.Is(unicode.Cf, r)
}

func lineBreak(r rune) bool {
	return r == '\u2028' || r == '\u2029'
}

func cleanBody(text string) string {
	text = strings.ToValidUTF8(text, string(replacement))
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	var out strings.Builder
	out.Grow(len(text))
	for _, r := range text {
		switch {
		case r == '\n' || r == '\t':
			out.WriteRune(r)
		case lineBreak(r):
			out.WriteByte('\n')
		case unwanted(r):
		default:
			out.WriteRune(r)
		}
	}
	lines := strings.Split(out.String(), "\n")
	for i, line := range lines {
		lines[i] = strings.TrimRight(line, " \t")
	}
	return collapseBlankLines(strings.Join(lines, "\n"))
}

func collapseBlankLines(text string) string {
	var out strings.Builder
	out.Grow(len(text))
	newlines := 0
	for _, r := range text {
		if r == '\n' {
			newlines++
			if newlines > 2 {
				continue
			}
		} else {
			newlines = 0
		}
		out.WriteRune(r)
	}
	return strings.TrimSpace(out.String())
}

func cleanLine(text string, limit int) string {
	text = strings.ToValidUTF8(text, string(replacement))
	text = strings.Map(func(r rune) rune {
		switch {
		case unicode.IsSpace(r) || lineBreak(r):
			return ' '
		case unwanted(r):
			return -1
		}
		return r
	}, text)
	return truncateRunes(strings.Join(strings.Fields(text), " "), limit)
}

func truncateRunes(text string, limit int) string {
	if utf8.RuneCountInString(text) <= limit {
		return text
	}
	count := 0
	for index := range text {
		if count == limit {
			return text[:index]
		}
		count++
	}
	return text
}

func truncateBytes(text string, limit int) (string, bool) {
	if len(text) <= limit {
		return text, false
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut], true
}

var wordDecoder = &mime.WordDecoder{CharsetReader: charset.NewReaderLabel}

const maxHeaderInput = 16 << 10

func decodeHeaderText(value string) string {
	value, _ = truncateBytes(value, maxHeaderInput)
	decoded, err := wordDecoder.DecodeHeader(value)
	if err != nil {
		return value
	}
	return decoded
}

func headerText(value string, limit int) string {
	return cleanLine(decodeHeaderText(value), limit)
}
