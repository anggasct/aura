package textsearch

import (
	"math"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestTokenizeExtractsBoundedUnicodeWords(t *testing.T) {
	got := Tokenize("eh, minggu lalu! kita bahas rencana liburan ke Jepang (2026) café? " + strings.Repeat("a", 65))
	want := []string{"eh", "minggu", "lalu", "kita", "bahas", "rencana", "liburan", "ke", "Jepang", "2026", "café"}
	if !slices.Equal(got, want) {
		t.Fatalf("Tokenize() = %v, want %v", got, want)
	}
	if empty := Tokenize("!!! ((())) ---"); len(empty) != 0 {
		t.Errorf("punctuation-only Tokenize() = %v, want empty", empty)
	}
	many := Tokenize(strings.Repeat("word ", 40))
	if len(many) != maxTokens {
		t.Errorf("len(Tokenize(40 words)) = %d, want %d", len(many), maxTokens)
	}
}

func TestFilterStopwordsDropsFillersAndKeepsAllStopwordQueries(t *testing.T) {
	got := FilterStopwords([]string{"eh", "minggu", "lalu", "kita", "bahas", "rencana", "liburan", "ke", "Jepang", "kan"})
	want := []string{"minggu", "lalu", "bahas", "rencana", "liburan", "Jepang"}
	if !slices.Equal(got, want) {
		t.Fatalf("FilterStopwords() = %v, want %v", got, want)
	}
	allStops := []string{"The", "and", "yang", "ini"}
	preserved := FilterStopwords(allStops)
	if !slices.Equal(preserved, allStops) {
		t.Errorf("FilterStopwords(all stopwords) = %v, want %v", preserved, allStops)
	}
	if empty := FilterStopwords(nil); len(empty) != 0 {
		t.Errorf("FilterStopwords(nil) = %v, want empty", empty)
	}
}

func TestFTSQueriesQuoteAndJoinTokens(t *testing.T) {
	tokens := []string{"rencana", `liburan"2026`, "Jepang"}
	if got, want := FTSAndQuery(tokens), `"rencana" "liburan""2026" "Jepang"`; got != want {
		t.Errorf("FTSAndQuery() = %q, want %q", got, want)
	}
	if got, want := FTSOrQuery(tokens), `"rencana" OR "liburan""2026" OR "Jepang"`; got != want {
		t.Errorf("FTSOrQuery() = %q, want %q", got, want)
	}
}

func TestRecencyScoreDecaysByHalfLife(t *testing.T) {
	now := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	if got := RecencyScore(now, now, DefaultHalfLifeDays); math.Abs(got-1.0) > 1e-12 {
		t.Errorf("age=0 score = %v, want 1.0", got)
	}
	if got := RecencyScore(now, now.Add(time.Hour), DefaultHalfLifeDays); math.Abs(got-1.0) > 1e-12 {
		t.Errorf("future score = %v, want 1.0", got)
	}
	if got := RecencyScore(now, now.Add(-30*24*time.Hour), DefaultHalfLifeDays); math.Abs(got-0.5) > 1e-12 {
		t.Errorf("age=30d score = %v, want 0.5", got)
	}
	if got := RecencyScore(now, now.Add(-120*24*time.Hour), DefaultHalfLifeDays); math.Abs(got-0.0625) > 1e-12 {
		t.Errorf("age=120d score = %v, want 0.0625", got)
	}
	if got := RecencyScore(now, now.Add(-24*time.Hour), 0); got != 1.0 {
		t.Errorf("non-positive halfLife score = %v, want 1.0", got)
	}
}

func TestNormalizeBM25AndCombineScoreAvoidNaN(t *testing.T) {
	if got := NormalizeBM25(-2.5, 2.5); got != 1.0 || math.IsNaN(got) {
		t.Errorf("NormalizeBM25(-2.5, 2.5) = %v, want 1.0", got)
	}
	if got := NormalizeBM25(0, 0); got != 1.0 || math.IsNaN(got) {
		t.Errorf("NormalizeBM25(0, 0) = %v, want 1.0", got)
	}
	if got := NormalizeBM25(-1.0, 4.0); math.Abs(got-0.25) > 1e-12 {
		t.Errorf("NormalizeBM25(-1.0, 4.0) = %v, want 0.25", got)
	}
	if got := CombineScore(0.8, 0.4); math.Abs(got-0.7) > 1e-12 {
		t.Errorf("CombineScore(0.8, 0.4) = %v, want 0.7", got)
	}
}
