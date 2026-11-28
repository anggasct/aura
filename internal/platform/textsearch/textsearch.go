package textsearch

import (
	"math"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	DefaultHalfLifeDays = 30.0
	maxTokenRunes       = 64
	maxTokens           = 32
	bm25Weight          = 0.75
	recencyWeight       = 0.25
)

func isWordChar(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r > 127
}

func isStopword(token string) bool {
	switch strings.ToLower(token) {
	case "the", "and", "for", "with", "that", "this", "from", "are", "was", "were", "you", "your",
		"yang", "dan", "di", "ke", "dari", "ini", "itu", "aku", "saya", "kita", "kami",
		"ya", "kan", "eh", "dong", "nih", "loh", "ada", "atau", "untuk", "pada", "juga",
		"dengan", "sudah", "belum", "lagi", "tadi", "bisa":
		return true
	default:
		return false
	}
}

func Tokenize(raw string) []string {
	terms := strings.FieldsFunc(raw, func(r rune) bool { return !isWordChar(r) })
	tokens := make([]string, 0, min(len(terms), maxTokens))
	for _, term := range terms {
		term = strings.TrimSpace(term)
		if term == "" || utf8.RuneCountInString(term) > maxTokenRunes {
			continue
		}
		tokens = append(tokens, term)
		if len(tokens) >= maxTokens {
			break
		}
	}
	return tokens
}

func FilterStopwords(tokens []string) []string {
	if len(tokens) == 0 {
		return nil
	}
	filtered := make([]string, 0, len(tokens))
	for _, token := range tokens {
		if !isStopword(token) {
			filtered = append(filtered, token)
		}
	}
	if len(filtered) == 0 {
		return slices.Clone(tokens)
	}
	return filtered
}

func quoteToken(token string) string {
	return `"` + strings.ReplaceAll(token, `"`, `""`) + `"`
}

func joinFTS(tokens []string, sep string) string {
	quoted := make([]string, 0, len(tokens))
	for _, token := range tokens {
		if token == "" {
			continue
		}
		quoted = append(quoted, quoteToken(token))
	}
	return strings.Join(quoted, sep)
}

func FTSAndQuery(tokens []string) string {
	return joinFTS(tokens, " ")
}

func FTSOrQuery(tokens []string) string {
	return joinFTS(tokens, " OR ")
}

func BM25Magnitude(rank float64) float64 {
	if math.IsNaN(rank) || math.IsInf(rank, 0) {
		return 0
	}
	return max(0, -rank)
}

func NormalizeBM25(rank, maxMag float64) float64 {
	if maxMag <= 0 || math.IsNaN(maxMag) || math.IsInf(maxMag, 0) {
		return 1.0
	}
	return BM25Magnitude(rank) / maxMag
}

func RecencyScore(now, ts time.Time, halfLifeDays float64) float64 {
	if halfLifeDays <= 0 || math.IsNaN(halfLifeDays) || math.IsInf(halfLifeDays, 0) {
		return 1.0
	}
	ageDays := max(0, now.Sub(ts).Hours()/24)
	return math.Exp(-math.Ln2 * ageDays / halfLifeDays)
}

func CombineScore(bm25Norm, recency float64) float64 {
	return bm25Weight*bm25Norm + recencyWeight*recency
}
