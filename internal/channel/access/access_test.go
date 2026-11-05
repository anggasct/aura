package access

import "testing"

func TestBudgetDropReason(t *testing.T) {
	cases := map[string]string{
		BudgetRefusalRateLimited: DropRateLimited,
		BudgetRefusalExhausted:   DropBudgetExhausted,
		"anything_else":          DropBudgetExhausted,
	}
	for refusal, want := range cases {
		if got := BudgetDropReason(refusal); got != want {
			t.Errorf("BudgetDropReason(%q) = %q, want %q", refusal, got, want)
		}
	}
}
