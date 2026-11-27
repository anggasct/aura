package discord

import (
	"context"
	"time"

	"github.com/anggasct/aura/internal/channels/kit/approval"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/principal"
)

const proposalCommandQuote = "`"

func proposalContent(card *approval.ProposalCard) string {
	return approval.ProposalHeading(card) + "\n" + fenceBlock(card.Plan)
}

func (a *Adapter) OfferProposal(ctx context.Context, card approval.ProposalCard, decide approval.ProposalDecider) error {
	if decide == nil {
		return fault.New(ErrorCodeInvalidArgument, "proposal decider must not be nil")
	}
	owners := a.owners.OwnerExternalIDs(principal.ChannelDiscord)
	if len(owners) == 0 {
		return approvalUnavailable("owner has no discord identity")
	}
	token, err := a.tokenRef.Resolve()
	if err != nil {
		return approvalUnavailable("bot token is unavailable")
	}
	dmChannel, err := a.ownerDMChannel(ctx, token, owners[0])
	if err != nil {
		return approvalUnavailable("owner dm channel is unavailable")
	}
	content := proposalContent(&card)
	if len([]rune(content)) > discordMessageLimit {
		if err := sendText(ctx, a.httpClient, a.restBase, token, dmChannel, approval.ProposalTooLongNotice(&card, proposalCommandQuote)); err != nil {
			return approvalUnavailable("proposal notice delivery failed")
		}
		return nil
	}
	expiresAt := time.Now().Add(approval.ProposalCardTTL)
	binding := approvalBinding{
		principal: principal.Namespaced(principal.ChannelDiscord, owners[0]),
		class:     principal.ClassOwner,
		actor:     owners[0],
		channelID: dmChannel,
	}
	tokenID, pending, err := a.approvals.Issue(binding.shared(), expiresAt)
	if err != nil {
		return approvalUnavailable("approval token mint failed")
	}
	cardID, err := a.postApprovalCard(ctx, token, dmChannel, content, tokenID, pending)
	if err != nil {
		a.approvals.Drop(tokenID, pending)
		return approvalUnavailable("proposal card delivery failed")
	}
	go a.awaitProposal(ctx, token, tokenID, pending, cardRef{channel: dmChannel, message: cardID}, card.ID, decide)
	return nil
}

func (a *Adapter) awaitProposal(ctx context.Context, token, tokenID string, pending *approval.Pending, card cardRef, proposalID string, decide approval.ProposalDecider) {
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
