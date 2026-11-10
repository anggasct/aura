package connector

import (
	"encoding/base64"
	"strings"
	"unicode/utf8"
)

const (
	crlf            = "\r\n"
	subjectPrefix   = "Subject: "
	encodedOpen     = "=?UTF-8?B?"
	encodedClose    = "?="
	maxEncodedWord  = 75
	firstChunkBytes = (foldWidth - len(subjectPrefix) - len(encodedOpen) - len(encodedClose)) / 4 * 3
	laterChunkBytes = (maxEncodedWord - len(encodedOpen) - len(encodedClose)) / 4 * 3
)

func hasControl(text string) bool {
	for i := range len(text) {
		if text[i] < 0x20 || text[i] == 0x7f {
			return true
		}
	}
	return false
}

func validSubject(subject string) bool {
	return subject != "" && len(subject) <= maxSubjectBytes && utf8.ValidString(subject) && !hasControl(subject) && !hasC1(subject) && strings.TrimSpace(subject) != ""
}

func hasC1(text string) bool {
	for _, r := range text {
		if r >= 0x80 && r <= 0x9f {
			return true
		}
	}
	return false
}

func subjectHeader(subject string) string {
	if folded, ok := foldPlainSubject(subject); ok {
		return folded
	}
	return encodedSubject(subject)
}

func plainSubject(subject string) bool {
	if strings.Contains(subject, "=?") || strings.Contains(subject, "  ") || subject[0] == ' ' || subject[len(subject)-1] == ' ' {
		return false
	}
	for i := range len(subject) {
		if subject[i] < 0x20 || subject[i] > 0x7e {
			return false
		}
	}
	return true
}

func foldPlainSubject(subject string) (string, bool) {
	if !plainSubject(subject) {
		return "", false
	}
	return foldTokens(subjectPrefix, strings.Split(subject, " "), "")
}

func encodedSubject(subject string) string {
	var out strings.Builder
	out.WriteString(subjectPrefix)
	rest := subject
	limit := firstChunkBytes
	for first := true; rest != ""; first = false {
		if !first {
			out.WriteString(crlf + " ")
		}
		cut := 0
		for cut < len(rest) {
			_, size := utf8.DecodeRuneInString(rest[cut:])
			if cut+size > limit {
				break
			}
			cut += size
		}
		out.WriteString(encodedOpen + base64.StdEncoding.EncodeToString([]byte(rest[:cut])) + encodedClose)
		rest = rest[cut:]
		limit = laterChunkBytes
	}
	return out.String()
}

func foldTokens(prefix string, tokens []string, suffix string) (string, bool) {
	var out strings.Builder
	out.WriteString(prefix)
	width := len(prefix)
	fits := true
	for i, token := range tokens {
		piece := token
		if i < len(tokens)-1 {
			piece += suffix
		}
		switch {
		case i == 0:
		case width+1+len(piece) <= foldWidth:
			out.WriteByte(' ')
			width++
		default:
			out.WriteString(crlf + " ")
			width = 1
		}
		if width+len(piece) > foldWidth {
			fits = false
		}
		out.WriteString(piece)
		width += len(piece)
	}
	return out.String(), fits
}

func foldList(name string, items []string, suffix string) string {
	folded, _ := foldTokens(name+": ", items, suffix)
	return folded
}
