package access

import (
	"strings"
	"testing"
)

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

func TestHandoffOwnerNotice(t *testing.T) {
	notice := HandoffOwnerNotice("Main (main)", "main/telegram:pocket/chat:1", "telegram:1", "  "+strings.Repeat("r", handoffReasonRunes+10)+"  ")
	lines := strings.Split(notice, "\n")
	want := []string{
		"handoff requested",
		"persona: Main (main)",
		"session: main/telegram:pocket/chat:1",
		"principal: telegram:1",
		"reason: " + strings.Repeat("r", handoffReasonRunes),
		"reply: aura reply main/telegram:pocket/chat:1 <text>",
		"release: aura handoff release main/telegram:pocket/chat:1",
	}
	if len(lines) != len(want) {
		t.Fatalf("notice has %d lines, want %d: %q", len(lines), len(want), notice)
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Errorf("line %d = %q, want %q", i, lines[i], want[i])
		}
	}
}
