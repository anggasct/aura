package runtimeingress

import (
	"strings"
	"unicode/utf8"
)

const MaxQueryRunes = 500

func JoinedText(parts []InputPart) string {
	texts := make([]string, 0, len(parts))
	for i := range parts {
		if trimmed := strings.TrimSpace(parts[i].Text); trimmed != "" {
			texts = append(texts, trimmed)
		}
	}
	return strings.Join(texts, "\n")
}

func QueryText(parts []InputPart) string {
	query := JoinedText(parts)
	if utf8.RuneCountInString(query) > MaxQueryRunes {
		query = string([]rune(query)[:MaxQueryRunes])
	}
	return query
}
