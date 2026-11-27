package approval

import (
	"context"
	"time"

	policy "github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/tools"
)

const (
	ProposalCardTTL    = 24 * time.Hour
	proposalCardHeader = "workflow proposal "
)

type ProposalDecider func(ctx context.Context, approved bool) (string, error)

type ProposalCard struct {
	ID      string
	Plan    string
	Tainted bool
}

func ProposalHeading(card *ProposalCard) string {
	heading := proposalCardHeader + tools.EscapeDisplayText(card.ID)
	if card.Tainted {
		heading += "\n" + policy.UntrustedInputNotice
	}
	return heading
}

func ProposalTooLongNotice(card *ProposalCard, commandQuote string) string {
	id := tools.EscapeDisplayText(card.ID)
	notice := proposalCardHeader + id + " is too long for a card. Review it with " + commandQuote + "aura workflow proposal show " + id + commandQuote + " and approve it there."
	if card.Tainted {
		notice += "\n" + policy.UntrustedInputNotice
	}
	return notice
}
