package approval

import (
	"testing"

	policy "github.com/anggasct/aura/internal/platform/approval"
)

func TestProposalHeadingPlacesTheUntrustedLineRightAfterTheHeader(t *testing.T) {
	cases := []struct {
		name string
		card ProposalCard
		want string
	}{
		{"trusted input", ProposalCard{ID: "prop_1"}, "workflow proposal prop_1"},
		{"untrusted input", ProposalCard{ID: "prop_1", Tainted: true}, "workflow proposal prop_1\n" + policy.UntrustedInputNotice},
		{"identifier with control characters", ProposalCard{ID: "prop\n1"}, "workflow proposal prop\\n1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ProposalHeading(&tc.card); got != tc.want {
				t.Fatalf("ProposalHeading() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestProposalTooLongNoticeQuotesTheCommandPerChannel(t *testing.T) {
	cases := []struct {
		name  string
		card  ProposalCard
		quote string
		want  string
	}{
		{"backticked command", ProposalCard{ID: "prop_2"}, "`", "workflow proposal prop_2 is too long for a card. Review it with `aura workflow proposal show prop_2` and approve it there."},
		{"bare command", ProposalCard{ID: "prop_2"}, "", "workflow proposal prop_2 is too long for a card. Review it with aura workflow proposal show prop_2 and approve it there."},
		{"untrusted input", ProposalCard{ID: "prop_2", Tainted: true}, "", "workflow proposal prop_2 is too long for a card. Review it with aura workflow proposal show prop_2 and approve it there.\n" + policy.UntrustedInputNotice},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ProposalTooLongNotice(&tc.card, tc.quote); got != tc.want {
				t.Fatalf("ProposalTooLongNotice() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestProposalCardTTLIsOneDay(t *testing.T) {
	if got := ProposalCardTTL.Hours(); got != 24 {
		t.Fatalf("ProposalCardTTL = %v hours, want 24", got)
	}
}
