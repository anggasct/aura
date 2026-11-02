package discord

import (
	"context"
	"time"

	"github.com/anggasct/aura/internal/principal"
)

const (
	ProposalCardTTL    = 24 * time.Hour
	proposalPlanRunes  = 1500
	proposalCardHeader = "workflow proposal "
)

type ProposalDecider func(ctx context.Context, approved bool) (string, error)

func (a *Adapter) OfferProposal(ctx context.Context, proposalID, plan string, decide ProposalDecider) error {
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
	expiresAt := time.Now().Add(ProposalCardTTL)
	binding := approvalBinding{
		principal: principal.Namespaced(principal.ChannelDiscord, owners[0]),
		class:     principal.ClassOwner,
		actor:     owners[0],
		channelID: dmChannel,
	}
	tokenID, pending, err := a.issueApproval(&binding, expiresAt)
	if err != nil {
		return Errorf(ErrorCodeApprovalUnavailable, "approval token mint failed")
	}
	content := proposalCardHeader + proposalID + "\n" + truncateRunes(plan, proposalPlanRunes)
	cardID, err := a.postApprovalCard(ctx, token, dmChannel, content, tokenID, &binding, expiresAt)
	if err != nil {
		a.dropApproval(tokenID, pending)
		return Errorf(ErrorCodeApprovalUnavailable, "proposal card delivery failed")
	}
	a.trackApprovalCard(pending, dmChannel, cardID)
	go a.awaitProposal(ctx, token, tokenID, pending, expiresAt, proposalID, dmChannel, decide)
	return nil
}

func (a *Adapter) awaitProposal(ctx context.Context, token, tokenID string, pending *pendingApproval, expiresAt time.Time, proposalID, dmChannel string, decide ProposalDecider) {
	outcome, timedOut, err := a.awaitApproval(ctx, token, tokenID, pending, expiresAt)
	if timedOut || err != nil {
		return
	}
	result, decideErr := decide(ctx, outcome.accepted)
	if decideErr != nil {
		result = "proposal " + proposalID + " could not be decided: " + decideErr.Error()
	}
	a.postNotice(ctx, token, dmChannel, result)
}
