package context

import (
	"encoding/json"
	"strings"
	"unicode/utf8"
)

var summaryKeys = []string{"goals", "decisions", "constraints", "open_work", "facts"}

func OutputByteBound(maxOutputTokens int) int {
	return maxOutputTokens * outputBytesPerToken
}

func ValidateContent(text string, maxOutputTokens int) (*SummaryContent, error) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return nil, Errorf(ErrorCodeSummaryInvalid, "summary output is empty")
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(trimmed), &raw); err != nil {
		return nil, Errorf(ErrorCodeSummaryInvalid, "summary output is not a JSON object")
	}
	if len(raw) != len(summaryKeys) {
		return nil, Errorf(ErrorCodeSummaryInvalid, "summary object carries unexpected keys")
	}
	content := &SummaryContent{}
	targets := map[string]*[]string{
		"goals":       &content.Goals,
		"decisions":   &content.Decisions,
		"constraints": &content.Constraints,
		"open_work":   &content.OpenWork,
		"facts":       &content.Facts,
	}
	entries := 0
	for _, key := range summaryKeys {
		encoded, ok := raw[key]
		if !ok {
			return nil, Errorf(ErrorCodeSummaryInvalid, "summary object misses a required key")
		}
		var values []string
		if err := json.Unmarshal(encoded, &values); err != nil || values == nil {
			return nil, Errorf(ErrorCodeSummaryInvalid, "summary value is not a string array")
		}
		for _, value := range values {
			if err := checkEntry(value); err != nil {
				return nil, err
			}
			entries++
		}
		*targets[key] = values
	}
	if entries == 0 {
		return nil, Errorf(ErrorCodeSummaryInvalid, "summary carries no entries")
	}
	if len(trimmed) > OutputByteBound(maxOutputTokens) {
		return nil, Errorf(ErrorCodeSummaryInvalid, "summary exceeds the output bound")
	}
	return content, nil
}

func checkEntry(value string) error {
	if !utf8.ValidString(value) || strings.ContainsRune(value, 0) {
		return Errorf(ErrorCodeSummaryInvalid, "summary entry is not clean text")
	}
	if strings.TrimSpace(value) == "" {
		return Errorf(ErrorCodeSummaryInvalid, "summary entry is empty")
	}
	if len(value) > maxEntryChars {
		return Errorf(ErrorCodeSummaryInvalid, "summary entry exceeds the entry bound")
	}
	if strings.ContainsAny(value, "{}<>`") {
		return Errorf(ErrorCodeSummaryInvalid, "summary entry carries machine syntax")
	}
	return nil
}

func canonicalContent(content *SummaryContent) (string, error) {
	encoded, err := json.Marshal(content)
	if err != nil {
		return "", Errorf(ErrorCodeSummaryInvalid, "summary content is not encodable")
	}
	return string(encoded), nil
}

func countedContent(counter TokenCounter, content *SummaryContent) (int, error) {
	encoded, err := canonicalContent(content)
	if err != nil {
		return 0, err
	}
	return sumCounts(counter, []string{encoded})
}
