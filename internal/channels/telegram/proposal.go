package telegram

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/anggasct/aura/internal/channels/kit/approval"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/principal"
	runtimechannelhost "github.com/anggasct/aura/internal/platform/turn/channelhost"
)

const entityPre = "pre"

type proposalFlow struct {
	card       openedCard
	proposalID string
	decide     approval.ProposalDecider
}

func proposalCardText(card *approval.ProposalCard) (string, []messageEntity) {
	head := approval.ProposalHeading(card) + "\n"
	return head + card.Plan, []messageEntity{{Type: entityPre, Offset: utf16Len(head), Length: utf16Len(card.Plan)}}
}

func (a *Adapter) OfferProposal(ctx context.Context, card approval.ProposalCard, decide approval.ProposalDecider) error {
	if decide == nil {
		return fault.New(ErrorCodeInvalidArgument, "proposal decider must not be nil")
	}
	ownerChat, ok := a.ownerChatID()
	if !ok {
		return approvalUnavailable("owner has no telegram identity")
	}
	token, ok := a.botToken()
	if !ok {
		return approvalUnavailable("bot token is unavailable")
	}
	dest := destination{chatID: ownerChat}
	text, entities := proposalCardText(&card)
	if utf16Len(text) > messageLimit {
		if err := sendPlainText(ctx, a.bot, token, dest, approval.ProposalTooLongNotice(&card, "")); err != nil {
			return approvalUnavailable("proposal notice delivery failed")
		}
		return nil
	}
	actor := strconv.FormatInt(ownerChat, 10)
	expiresAt := time.Now().Add(a.proposalTTL)
	binding := approval.Binding{
		Principal: principal.Namespaced(principal.ChannelTelegram, actor),
		Class:     principal.ClassOwner,
		Actor:     actor,
		Scope:     scopeOf(dest),
	}
	opened, err := a.openCard(ctx, token, &cardRequest{binding: &binding, dest: dest, text: text, entities: entities, expiresAt: expiresAt})
	if errors.Is(err, runtimechannelhost.ErrDeliveryAmbiguous) {
		return err
	}
	if err != nil {
		return approvalUnavailable("proposal card delivery failed")
	}
	go a.settleProposal(ctx, token, &proposalFlow{card: opened, proposalID: card.ID, decide: decide})
	return nil
}

func (a *Adapter) settleProposal(ctx context.Context, token string, flow *proposalFlow) {
	outcome, timedOut, err := a.awaitApproval(ctx, token, &flow.card)
	if timedOut || err != nil {
		return
	}
	result, decideErr := flow.decide(ctx, outcome.Accepted)
	if decideErr != nil {
		result = "proposal " + flow.proposalID + " could not be decided: " + decideErr.Error()
	}
	a.postNotice(ctx, flow.card.dest, result)
}
