package discord

import (
	"context"
	"strings"
	"time"

	"github.com/anggasct/aura/internal/channels/kit/approval"
	"github.com/anggasct/aura/internal/channels/kit/status"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/tools"
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
	tokenID, pending, err := a.approvals.Issue(binding.shared(), expiresAt)
	if err != nil {
		return Errorf(ErrorCodeApprovalUnavailable, "approval token mint failed")
	}
	lines := strings.Split(plan, "\n")
	for i, line := range lines {
		lines[i] = tools.EscapeDisplayText(line)
	}
	content := proposalCardHeader + tools.EscapeDisplayText(proposalID) + "\n" + fenceBlock(status.TruncateRunes(strings.Join(lines, "\n"), proposalPlanRunes))
	cardID, err := a.postApprovalCard(ctx, token, dmChannel, content, tokenID, pending)
	if err != nil {
		a.approvals.Drop(tokenID, pending)
		return Errorf(ErrorCodeApprovalUnavailable, "proposal card delivery failed")
	}
	go a.awaitProposal(ctx, token, tokenID, pending, cardRef{channel: dmChannel, message: cardID}, proposalID, decide)
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
