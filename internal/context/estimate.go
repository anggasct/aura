package context

import (
	"math"
	"unicode/utf8"
)

type AccountingClass string

const (
	AccountingExact     AccountingClass = "exact"
	AccountingEstimated AccountingClass = "estimated"
)

type TokenCounter interface {
	Count(text string) int
	Class() AccountingClass
	Name() string
}

type ConservativeEstimator struct{}

func (ConservativeEstimator) Name() string { return "utf8-conservative-v1" }

func (ConservativeEstimator) Class() AccountingClass { return AccountingEstimated }

func (ConservativeEstimator) Count(text string) int {
	if text == "" {
		return 0
	}
	var ascii, other int
	for _, r := range text {
		if r < utf8.RuneSelf {
			ascii++
		} else {
			other++
		}
	}
	total := other + (ascii+3)/4
	return max(total, 1)
}

func checkedAdd(a, b int) (int, error) {
	if b > 0 && a > math.MaxInt-b {
		return 0, Errorf(ErrorCodeInvalidArgument, "token total overflows")
	}
	if b < 0 && a < math.MinInt-b {
		return 0, Errorf(ErrorCodeInvalidArgument, "token total underflows")
	}
	return a + b, nil
}

func sumCounts(counter TokenCounter, texts []string) (int, error) {
	total := 0
	for _, text := range texts {
		count := counter.Count(text)
		if count < 0 {
			return 0, Errorf(ErrorCodeInvalidArgument, "counter %q returned a negative count", counter.Name())
		}
		next, err := checkedAdd(total, count)
		if err != nil {
			return 0, err
		}
		total = next
	}
	return total, nil
}
