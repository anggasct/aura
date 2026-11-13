package discord

import (
	"context"
	"time"

	"github.com/anggasct/aura/internal/channels/kit/approval"
	policy "github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/principal"
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

func (c ProposalCard) content() string {
	heading := proposalCardHeader + tools.EscapeDisplayText(c.ID)
	if c.Tainted {
		heading += "\n" + policy.UntrustedInputNotice
	}
	return heading + "\n" + fenceBlock(c.Plan)
}

func (c ProposalCard) tooLongNotice() string {
	id := tools.EscapeDisplayText(c.ID)
	notice := proposalCardHeader + id + " is too long for a card. Review it with `aura workflow proposal show " + id + "` and approve it there."
	if c.Tainted {
		notice += "\n" + policy.UntrustedInputNotice
	}
	return notice
}

func (a *Adapter) OfferProposal(ctx context.Context, card ProposalCard, decide ProposalDecider) error {
	if decide == nil {
		return Errorf(ErrorCodeInvalidArgument, "proposal decider must not be nil")
	}
	owners := a.owners.OwnerExternalIDs(principal.ChannelDiscord)
	if len(owners) == 0 {
		return Errorf(ErrorCodeApprovalUnavailable, "owner has no discord identity")
	}
	token, err := a.tokenRef.Resolve()
	if err != nil {
		return Errorf(ErrorCodeApprovalUnavailable, "bot token is unavailable")
	}
	dmChannel, err := a.ownerDMChannel(ctx, token, owners[0])
	if err != nil {
		return Errorf(ErrorCodeApprovalUnavailable, "owner dm channel is unavailable")
	}
	content := card.content()
	if len([]rune(content)) > discordMessageLimit {
		a.postNotice(ctx, token, dmChannel, card.tooLongNotice())
		return nil
	}
	expiresAt := time.Now().Add(ProposalCardTTL)
	binding := approvalBinding{
		principal: principal.Namespaced(principal.ChannelDiscord, owners[0]),
		class:     principal.ClassOwner,
		actor:     owners[0],
		channelID: dmChannel,
	}
	tokenID, pending, err := a.approvals.Issue(binding.shared(), expiresAt)
	if err != nil {
		return Errorf(ErrorCodeApprovalUnavailable, "approval token mint failed")
	}
	cardID, err := a.postApprovalCard(ctx, token, dmChannel, content, tokenID, pending)
	if err != nil {
		a.approvals.Drop(tokenID, pending)
		return Errorf(ErrorCodeApprovalUnavailable, "proposal card delivery failed")
	}
	go a.awaitProposal(ctx, token, tokenID, pending, cardRef{channel: dmChannel, message: cardID}, card.ID, decide)
	return nil
}

func (a *Adapter) awaitProposal(ctx context.Context, token, tokenID string, pending *approval.Pending, card cardRef, proposalID string, decide ProposalDecider) {
	outcome, timedOut, err := a.awaitApproval(ctx, token, tokenID, pending, card)
	if timedOut || err != nil {
		return
	}
	result, decideErr := decide(ctx, outcome.Accepted)
	if decideErr != nil {
		result = "proposal " + proposalID + " could not be decided: " + decideErr.Error()
	}
	a.postNotice(ctx, token, card.channel, result)
}
