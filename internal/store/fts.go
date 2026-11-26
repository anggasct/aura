package store

import "strings"

func isFTSWordChar(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r > 127
}

func sanitizeFTSQuery(raw string) (string, error) {
	if len([]rune(raw)) > 500 {
		return "", Errorf(ErrorCodeInvalidArgument, "search query exceeds length bounds")
	}
	terms := strings.FieldsFunc(raw, func(r rune) bool { return !isFTSWordChar(r) })
	cleaned := make([]string, 0, len(terms))
	for _, term := range terms {
		term = strings.TrimSpace(term)
		if term == "" || len([]rune(term)) > 64 {
			continue
		}
		cleaned = append(cleaned, `"`+strings.ReplaceAll(term, `"`, `""`)+`"`)
		if len(cleaned) >= 32 {
			break
		}
	}
	if len(cleaned) == 0 {
		return "", Errorf(ErrorCodeInvalidArgument, "search query carries no searchable terms")
	}
	return strings.Join(cleaned, " "), nil
}
