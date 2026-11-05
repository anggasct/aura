package telegram

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/anggasct/aura/internal/channel/approval"
	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/principal"
)

const maxTrackedTargets = 1024

type approvalTarget struct {
	binding approval.Binding
	dest    destination
}

type openedCard struct {
	tokenID   string
	pending   *approval.Pending
	dest      destination
	messageID int64
}

func scopeOf(dest destination) [2]string {
	return [2]string{strconv.FormatInt(dest.chatID, 10), strconv.FormatInt(dest.threadID, 10)}
}

func (a *Adapter) rememberTarget(sessionID string, target *approvalTarget) {
	a.targetsMu.Lock()
	defer a.targetsMu.Unlock()
	if len(a.targets) >= maxTrackedTargets {
		for drop := range a.targets {
			delete(a.targets, drop)
			break
		}
	}
	a.targets[sessionID] = *target
}

func (a *Adapter) lookupTarget(sessionID string) (approvalTarget, bool) {
	a.targetsMu.Lock()
	defer a.targetsMu.Unlock()
	target, ok := a.targets[sessionID]
	return target, ok
}

func (a *Adapter) HoldsApprovalTarget(sessionID string) bool {
	_, ok := a.lookupTarget(sessionID)
	return ok
}

func (a *Adapter) observeRouted(ctx context.Context, result string) {
	if a.access.ApprovalRouted != nil {
		a.access.ApprovalRouted(ctx, result)
	}
}

func (a *Adapter) Decide(ctx context.Context, prompt *approval.Prompt) (bool, error) {
	if ctx == nil {
		return false, Errorf(ErrorCodeInvalidArgument, "context must not be nil")
	}
	if prompt == nil {
		return false, Errorf(ErrorCodeInvalidArgument, "approval prompt must not be nil")
	}
	if strings.TrimSpace(prompt.ToolName) == "" || strings.TrimSpace(prompt.SessionID) == "" || strings.TrimSpace(prompt.PrincipalID) == "" {
		return false, Errorf(ErrorCodeInvalidArgument, "approval prompt is missing tool, session, or principal")
	}
	target, ok := a.lookupTarget(prompt.SessionID)
	if !ok || target.binding.Principal != prompt.PrincipalID {
		return false, nil
	}
	if target.binding.Principal != principal.OwnerID {
		return a.decideForOwner(ctx, prompt, &target)
	}
	return a.decideInChat(ctx, prompt, &target)
}

func (a *Adapter) decideInChat(ctx context.Context, prompt *approval.Prompt, target *approvalTarget) (bool, error) {
	expiresAt := prompt.ExpiresAt
	if expiresAt.IsZero() {
		expiresAt = time.Now().Add(approval.FallbackTTL)
	}
	if !time.Now().Before(expiresAt) {
		return false, nil
	}
	token, ok := a.botToken()
	if !ok {
		a.logger.WarnContext(ctx, "approval token unavailable", "component", "telegram", "account", a.account.Name)
		return false, ctx.Err()
	}
	card, err := a.openCard(ctx, token, &target.binding, target.dest, approval.RenderCard(prompt, "", nil), expiresAt)
	if err != nil {
		a.logger.WarnContext(ctx, "approval card delivery failed", "component", "telegram", "account", a.account.Name)
		return false, ctx.Err()
	}
	outcome, _, err := a.awaitApproval(ctx, token, &card)
	return outcome.Accepted, err
}

func (a *Adapter) decideForOwner(ctx context.Context, prompt *approval.Prompt, requester *approvalTarget) (bool, error) {
	persona := requester.binding.Persona
	definition, _ := a.personaDefinition(persona)
	if definition.ID == "" {
		definition.ID = persona
	}
	ownerChat, ok := a.ownerChatID()
	if !ok {
		a.observeRouted(ctx, approval.RoutedUnavailable)
		return false, Errorf(ErrorCodeApprovalUnavailable, "owner has no telegram identity")
	}
	token, ok := a.botToken()
	if !ok {
		a.observeRouted(ctx, approval.RoutedUnavailable)
		return false, Errorf(ErrorCodeApprovalUnavailable, "approval token is unavailable")
	}
	now := time.Now()
	expiresAt := now.Add(approval.OwnerTimeout(definition.ApprovalTimeout, a.access.ApprovalTimeout))
	if !prompt.ExpiresAt.IsZero() && prompt.ExpiresAt.Before(expiresAt) {
		expiresAt = prompt.ExpiresAt
	}
	if !now.Before(expiresAt) {
		return false, nil
	}
	ownerDest := destination{chatID: ownerChat}
	binding := approval.Binding{
		Principal: requester.binding.Principal,
		Class:     requester.binding.Class,
		Persona:   persona,
		Actor:     strconv.FormatInt(ownerChat, 10),
		Scope:     scopeOf(ownerDest),
		Standing:  approval.OffersStandingGrant(requester.binding.Class, a.access.Grants, a.access.StandingGrantTTL),
	}
	text := approval.RenderCard(prompt, approval.PersonaLabel(definition.ID, definition.Name), nil)
	card, err := a.openCard(ctx, token, &binding, ownerDest, text, expiresAt)
	if err != nil {
		a.observeRouted(ctx, approval.RoutedUnavailable)
		return false, Errorf(ErrorCodeApprovalUnavailable, "owner approval card delivery failed")
	}
	a.postNotice(ctx, requester.dest, definition.Message(config.MessageWaitingForOwner))
	outcome, timedOut, err := a.awaitApproval(ctx, token, &card)
	notify := context.WithoutCancel(ctx)
	switch {
	case timedOut || errors.Is(err, context.DeadlineExceeded):
		a.observeRouted(ctx, approval.RoutedTimeout)
		a.postNotice(notify, requester.dest, definition.Message(config.MessageApprovalTimeout))
		if err != nil {
			return false, err
		}
		return false, Errorf(ErrorCodeApprovalTimeout, "owner did not answer the approval in time")
	case err != nil:
		return false, err
	case outcome.Standing:
		a.observeRouted(ctx, approval.RoutedStanding)
		if recordErr := a.access.Grants.RecordStandingGrant(notify, &approval.StandingGrantRequest{
			PrincipalID: binding.Principal,
			AgentID:     persona,
			ToolName:    prompt.ToolName,
			OwnerID:     principal.Namespaced(principal.ChannelTelegram, binding.Actor),
			TTL:         a.access.StandingGrantTTL,
		}); recordErr != nil {
			a.logger.WarnContext(ctx, "standing grant record failed", "component", "telegram", "account", a.account.Name)
		}
		return true, nil
	case outcome.Accepted:
		a.observeRouted(ctx, approval.RoutedApproved)
		return true, nil
	default:
		a.observeRouted(ctx, approval.RoutedRejected)
		a.postNotice(notify, requester.dest, definition.Message(config.MessageApprovalDenied))
		return false, nil
	}
}

func (a *Adapter) openCard(ctx context.Context, token string, binding *approval.Binding, dest destination, text string, expiresAt time.Time) (openedCard, error) {
	tokenID, pending, err := a.approvals.Issue(binding, expiresAt)
	if err != nil {
		return openedCard{}, err
	}
	callbacks := a.approvals.Callbacks(tokenID, binding, expiresAt)
	row := []inlineButton{{Text: "Approve", CallbackData: callbacks.Approve}}
	if callbacks.Standing != "" {
		row = append(row, inlineButton{Text: approval.StandingGrantLabel(a.access.StandingGrantTTL), CallbackData: callbacks.Standing})
	}
	row = append(row, inlineButton{Text: "Reject", CallbackData: callbacks.Reject})
	call, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	sent, err := a.bot.sendMessage(call, token, &sendMessageRequest{
		ChatID:          dest.chatID,
		MessageThreadID: dest.threadID,
		Text:            text,
		ReplyMarkup:     &inlineKeyboard{Rows: [][]inlineButton{row}},
	})
	if err != nil {
		a.approvals.Drop(tokenID, pending)
		return openedCard{}, err
	}
	if sent.MessageID == 0 {
		a.approvals.Drop(tokenID, pending)
		return openedCard{}, Errorf(ErrorCodeDeliveryAmbiguous, "approval card post has no message id")
	}
	return openedCard{tokenID: tokenID, pending: pending, dest: dest, messageID: sent.MessageID}, nil
}

func (a *Adapter) awaitApproval(ctx context.Context, token string, card *openedCard) (approval.Outcome, bool, error) {
	defer a.clearKeyboard(context.WithoutCancel(ctx), token, card)
	return a.approvals.Await(ctx, card.tokenID, card.pending)
}

func (a *Adapter) clearKeyboard(ctx context.Context, token string, card *openedCard) {
	call, cancel := context.WithTimeout(ctx, noticeTimeout)
	defer cancel()
	if err := a.bot.clearKeyboard(call, token, card.dest.chatID, card.messageID); err != nil {
		a.logger.WarnContext(ctx, "approval keyboard removal failed", "component", "telegram", "account", a.account.Name)
	}
}

func (a *Adapter) handleCallback(ctx context.Context, query *callbackQuery) {
	resolved := a.resolveCallback(query)
	a.answerCallback(ctx, query)
	if !resolved {
		a.drop(ctx, callbackScope(query), dropCallbackRejected, true)
	}
}

func callbackScope(query *callbackQuery) string {
	if query.Message != nil && query.Message.Chat.Type == chatTypePrivate {
		return dropScopePrivate
	}
	return dropScopeChat
}

func (a *Adapter) resolveCallback(query *callbackQuery) bool {
	if query.ID == "" || query.From == nil || query.From.ID <= 0 || query.From.IsBot || query.Message == nil || query.Message.Chat.ID == 0 {
		return false
	}
	if !a.approvals.NoteSeen(query.ID) {
		return false
	}
	return a.approvals.Resolve(query.Data, &approval.Presser{
		Actor: strconv.FormatInt(query.From.ID, 10),
		Scope: scopeOf(query.Message.destination()),
	})
}

func (a *Adapter) answerCallback(ctx context.Context, query *callbackQuery) {
	if query.ID == "" {
		return
	}
	token, ok := a.botToken()
	if !ok {
		a.logger.WarnContext(ctx, "callback answer token unavailable", "component", "telegram", "account", a.account.Name)
		return
	}
	call, cancel := context.WithTimeout(ctx, noticeTimeout)
	defer cancel()
	if err := a.bot.answerCallbackQuery(call, token, query.ID); err != nil {
		a.logger.WarnContext(ctx, "callback answer failed", "component", "telegram", "account", a.account.Name)
	}
}
