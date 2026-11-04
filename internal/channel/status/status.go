package status

import (
	"encoding/json"
	"slices"
	"strings"

	"github.com/anggasct/aura/internal/secret"
)

const (
	MaxWords      = 6
	maxArgRunes   = 80
	maxTextRunes  = 280
	FailureMarker = "⚠️"
)

var fallbacks = [3]string{"Working…", "Still working…", "Wrapping up…"}

var secretKeywords = []string{"password", "secret", "token", "api_key", "apikey", "bearer", "authorization", "private_key", "client_secret"}

func TruncateWords(s string, n int) string {
	words := strings.Fields(s)
	if len(words) <= n {
		return strings.Join(words, " ")
	}
	return strings.Join(words[:n], " ")
}

func TruncateRunes(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n])
}

func Fallbacks() []string {
	return slices.Clone(fallbacks[:])
}

func Fallback(count int) string {
	if count < 0 {
		count = 0
	}
	return fallbacks[count%len(fallbacks)]
}

func FailureLine(activity string) string {
	activity = strings.TrimSpace(activity)
	activity = strings.TrimSuffix(activity, ".")
	activity = strings.TrimSpace(activity)
	if activity == "" {
		return FailureMarker
	}
	return FailureMarker + " " + activity
}

func ContainsSecretKeyword(s string) bool {
	lowered := strings.ToLower(s)
	for _, keyword := range secretKeywords {
		if strings.Contains(lowered, keyword) {
			return true
		}
	}
	return false
}

func HumanizeArgs(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	var decoded any
	if err := json.Unmarshal([]byte(trimmed), &decoded); err != nil {
		return ""
	}
	var parts []string
	var collect func(v any)
	collect = func(v any) {
		switch value := v.(type) {
		case string:
			if strings.TrimSpace(value) != "" {
				parts = append(parts, value)
			}
		case map[string]any:
			for _, item := range value {
				collect(item)
			}
		case []any:
			for _, item := range value {
				collect(item)
			}
		}
	}
	collect(decoded)
	if len(parts) == 0 {
		return ""
	}
	return TruncateRunes(strings.Join(parts, " "), maxArgRunes)
}

func Render(raw, args string, updateCount int, vault *secret.Vault) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return Fallback(updateCount)
	}
	short := TruncateWords(trimmed, MaxWords)
	short = TruncateRunes(strings.TrimSpace(short), maxTextRunes)
	if short == "" {
		return Fallback(updateCount)
	}
	if vault != nil {
		short = vault.Redact(short)
	}
	if ContainsSecretKeyword(short) {
		return Fallback(updateCount)
	}
	argText := HumanizeArgs(args)
	secretHit := false
	switch {
	case vault != nil && argText != "" && vault.Contains(argText):
		secretHit = true
	case vault != nil && args != "" && vault.Contains(args):
		secretHit = true
	case ContainsSecretKeyword(args) || ContainsSecretKeyword(argText):
		secretHit = true
	}
	if secretHit || argText == "" {
		return short
	}
	if vault != nil {
		argText = vault.Redact(argText)
	}
	return TruncateRunes(short+" "+argText, maxTextRunes)
}

func RenderFailure(activity string, vault *secret.Vault) string {
	activity = TruncateWords(strings.TrimSpace(activity), MaxWords)
	if vault != nil {
		activity = vault.Redact(activity)
	}
	if ContainsSecretKeyword(activity) {
		activity = ""
	}
	return FailureLine(activity)
}
