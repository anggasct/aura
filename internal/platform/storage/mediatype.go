package storage

import "strings"

func BaseMediaType(contentType string) string {
	head, _, _ := strings.Cut(contentType, ";")
	return strings.ToLower(strings.TrimSpace(head))
}
