package context

import (
	"strings"
	"testing"
	"unicode/utf8"
)

type exactCounter struct{}

func (exactCounter) Name() string           { return "test-exact-v1" }
func (exactCounter) Class() AccountingClass { return AccountingExact }
func (exactCounter) Count(text string) int  { return utf8.RuneCountInString(text) }

func TestEstimatorEmptyCountsZero(t *testing.T) {
	if got := (ConservativeEstimator{}).Count(""); got != 0 {
		t.Errorf("empty = %d", got)
	}
}

func TestEstimatorASCIIRatio(t *testing.T) {
	text := strings.Repeat("hello world ", 100)
	got := (ConservativeEstimator{}).Count(text)
	want := (len(text) + 3) / 4
	if got != want {
		t.Errorf("ascii = %d, want %d", got, want)
	}
}

func TestEstimatorNonASCIICoversRunes(t *testing.T) {
	text := strings.Repeat("日本語テスト", 50)
	got := (ConservativeEstimator{}).Count(text)
	if got < utf8.RuneCountInString(text) {
		t.Errorf("cjk = %d below rune count %d", got, utf8.RuneCountInString(text))
	}
}

func TestEstimatorMonotonicAndDeterministic(t *testing.T) {
	counter := ConservativeEstimator{}
	cases := []string{"", "a", "hello world", "日本語テスト混合 text 123", strings.Repeat("x", 1000)}
	last := -1
	for _, text := range cases {
		first, second := counter.Count(text), counter.Count(text)
		if first != second {
			t.Errorf("nondeterministic for %q", text)
		}
		if first < last {
			t.Errorf("not monotonic at %q", text)
		}
		last = first
	}
}

func TestEstimatorMarksEstimated(t *testing.T) {
	counter := ConservativeEstimator{}
	if counter.Class() != AccountingEstimated {
		t.Errorf("class = %q", counter.Class())
	}
	if counter.Name() == "" {
		t.Errorf("counter carries no identity")
	}
}
