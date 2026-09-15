package profile

import (
	"encoding/json"
	"strings"
	"unicode/utf8"
)

const (
	extractPromptV1      = "v1"
	maxSourceEventChars  = 8000
	maxSourceTotalChars  = 64000
	extractorMaxAttempts = 3
)

var sensitiveKeywords = []string{
	"health", "medical", "diagnosis", "disease", "doctor", "clinic", "hospital", "patient",
	"religion", "church", "mosque", "temple",
	"ethnicity", "ethnic", "race", "racial",
	"politic", "election", "vote",
	"sexual", "orientation",
	"biometric", "fingerprint", "retina",
	"password", "passwd", "secret", "token", "apikey", "api_key",
	"private key", "privatekey", "seed phrase", "credential",
	"ssn", "social security", "credit card", "card number",
	"bank account", "iban", "payment",
}

type SecretScanner interface {
	Contains(text string) bool
}

func SensitiveReason(category, key, value string, scanner SecretScanner) string {
	lowered := strings.ToLower(key + "\n" + value)
	for _, keyword := range sensitiveKeywords {
		if strings.Contains(lowered, keyword) {
			return "sensitive trait"
		}
	}
	if scanner != nil && (scanner.Contains(key) || scanner.Contains(value)) {
		return "secret-like value"
	}
	return ""
}

type RawCandidate struct {
	Category string
	Key      string
	Value    string
	Sources  []uint64
}

func ParseCandidates(text string) ([]RawCandidate, error) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return nil, Errorf(ErrorCodeProfileInvalid, "extractor output is empty")
	}
	var raw []struct {
		Category string   `json:"category"`
		Key      string   `json:"key"`
		Value    string   `json:"value"`
		Sources  []uint64 `json:"sources"`
	}
	if err := json.Unmarshal([]byte(trimmed), &raw); err != nil {
		return nil, Errorf(ErrorCodeProfileInvalid, "extractor output is not a candidate array")
	}
	candidates := make([]RawCandidate, 0, len(raw))
	for _, item := range raw {
		if strings.TrimSpace(item.Category) == "" || strings.TrimSpace(item.Key) == "" || strings.TrimSpace(item.Value) == "" || len(item.Sources) == 0 {
			continue
		}
		candidates = append(candidates, RawCandidate{
			Category: item.Category,
			Key:      item.Key,
			Value:    item.Value,
			Sources:  item.Sources,
		})
	}
	return candidates, nil
}

func ExtractPrompt(promptVersion string, maxOutputTokens int) (string, error) {
	if promptVersion != extractPromptV1 {
		return "", Errorf(ErrorCodeProfileInvalid, "unsupported extractor prompt version")
	}
	if maxOutputTokens <= 0 {
		return "", Errorf(ErrorCodeInvalidArgument, "output bound must be positive")
	}
	categories := strings.Join(Categories, ", ")
	return "Extract durable owner facts from the source turn events as a JSON array. " +
		"Each element has exactly category, key, value, sources. " +
		"Category is one of: " + categories + ". " +
		"Key names the fact in a few words; value states it plainly. " +
		"Sources lists the source event sequence numbers supporting the fact. " +
		"Never emit secrets, credentials, tokens, or sensitive personal traits. " +
		"Emit nothing but the JSON array.", nil
}

func truncateSource(text string, maxChars int) string {
	if utf8.RuneCountInString(text) <= maxChars {
		return text
	}
	runes := []rune(text)
	return string(runes[:maxChars])
}
