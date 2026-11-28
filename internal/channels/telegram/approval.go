package telegram

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/anggasct/aura/internal/channels/kit/approval"
	auraagent "github.com/anggasct/aura/internal/platform/agent"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/principal"
)

const (
	maxTrackedTargets          = 1024
	entityExpandableBlockquote = "expandable_blockquote"
)

type approvalTarget struct {
	binding approval.Binding
	dest    destination
}

type cardRequest struct {
	binding     *approval.Binding
	dest        destination
	text        string
	entities    []messageEntity
	buttons     approval.CardButtons
	persona     auraagent.Definition
	editOutcome bool
	expiresAt   time.Time
}

type openedCard struct {
	tokenID     string
	pending     *approval.Pending
	dest        destination
	messageID   int64
	text        string
	entities    []messageEntity
	persona     auraagent.Definition
	editOutcome bool
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

func validateApprovalPrompt(ctx context.Context, prompt *approval.Prompt) error {
	if ctx == nil {
		return fault.New(ErrorCodeInvalidArgument, "context must not be nil")
	}
	if prompt == nil {
		return fault.New(ErrorCodeInvalidArgument, "approval prompt must not be nil")
	}
	if strings.TrimSpace(prompt.ToolName) == "" || strings.TrimSpace(prompt.SessionID) == "" || strings.TrimSpace(prompt.PrincipalID) == "" {
		return fault.New(ErrorCodeInvalidArgument, "approval prompt is missing tool, session, or principal")
	}
	return nil
}

func (a *Adapter) Decide(ctx context.Context, prompt *approval.Prompt) (bool, error) {
	if err := validateApprovalPrompt(ctx, prompt); err != nil {
		return false, err
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

func (a *Adapter) DecideForOwner(ctx context.Context, prompt *approval.Prompt, requester *approval.Requester) (bool, error) {
	if err := validateApprovalPrompt(ctx, prompt); err != nil {
		return false, err
	}
	if requester == nil || requester.Principal == "" || requester.Principal != prompt.PrincipalID {
		return false, fault.New(ErrorCodeInvalidArgument, "approval requester is missing or does not match the prompt")
	}
	return a.decideForOwner(ctx, prompt, &approvalTarget{binding: approval.Binding{Principal: requester.Principal, Class: requester.Class, Persona: requester.Persona}})
}

func (a *Adapter) ServesPersona(persona string) bool {
	return a.router != nil && a.router.ServesPersona(auraagent.BindingChannelTelegram, a.account.Name, persona)
}

func (a *Adapter) cardPersona(personaID string) auraagent.Definition {
	definition, ok := a.personaDefinition(personaID)
	if !ok && personaID == "" && a.account != nil {
		definition, _ = a.personaDefinition(a.account.Persona)
	}
	if definition.ID == "" {
		definition.ID = personaID
	}
	return definition
}

func (a *Adapter) buildToolCard(prompt *approval.Prompt, persona *auraagent.Definition, standing bool) (text string, entities []messageEntity, buttons approval.CardButtons, err error) {
	rendered := approval.RenderCard(prompt, approval.CardOptions{Persona: persona})
	head := rendered.Header + "\n"
	text = head + rendered.Details
	entities = []messageEntity{{
		Type:   entityExpandableBlockquote,
		Offset: utf16Len(head),
		Length: utf16Len(rendered.Details),
	}}
	longest := approval.LongestOutcomeLine(&approval.OutcomeLineInput{
		Persona:     persona,
		StandingTTL: a.access.StandingGrantTTL,
		Location:    a.ownerLoc,
	}, standing, utf16Len)
	if utf16Len(text)+1+longest > messageLimit {
		return "", nil, approval.CardButtons{}, approvalUnavailable("approval card exceeds telegram message limit")
	}
	var standingTTL time.Duration
	if standing {
		standingTTL = a.access.StandingGrantTTL
	}
	return text, entities, approval.RenderButtons(persona, standingTTL), nil
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
	definition := a.cardPersona(target.binding.Persona)
	text, entities, buttons, err := a.buildToolCard(prompt, &definition, false)
	if err != nil {
		return false, err
	}
	card, err := a.openCard(ctx, token, &cardRequest{
		binding:     &target.binding,
		dest:        target.dest,
		text:        text,
		entities:    entities,
		buttons:     buttons,
		persona:     definition,
		editOutcome: true,
		expiresAt:   expiresAt,
	})
	if err != nil {
		a.logger.WarnContext(ctx, "approval card delivery failed", "component", "telegram", "account", a.account.Name)
		return false, ctx.Err()
	}
	outcome, _, err := a.awaitApproval(ctx, token, &card)
	return outcome.Accepted, err
}

func (a *Adapter) decideForOwner(ctx context.Context, prompt *approval.Prompt, requester *approvalTarget) (bool, error) {
	persona := requester.binding.Persona
	definition := a.cardPersona(persona)
	ownerChat, ok := a.ownerChatID()
	if !ok {
		a.observeRouted(ctx, approval.RoutedUnavailable)
		return false, approvalUnavailable("owner has no telegram identity")
	}
	token, ok := a.botToken()
	if !ok {
		a.observeRouted(ctx, approval.RoutedUnavailable)
		return false, approvalUnavailable("approval token is unavailable")
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
	standing := approval.OffersStandingGrant(prompt, requester.binding.Class, approval.StandingOffer{Grants: a.access.Grants, TTL: a.access.StandingGrantTTL})
	text, entities, buttons, err := a.buildToolCard(prompt, &definition, standing)
	if err != nil {
		a.observeRouted(ctx, approval.RoutedUnavailable)
		return false, err
	}
	binding := approval.Binding{
		Principal: requester.binding.Principal,
		Class:     requester.binding.Class,
		Persona:   persona,
		Actor:     strconv.FormatInt(ownerChat, 10),
		Scope:     scopeOf(ownerDest),
		Standing:  standing,
	}
	card, err := a.openCard(ctx, token, &cardRequest{
		binding:     &binding,
		dest:        ownerDest,
		text:        text,
		entities:    entities,
		buttons:     buttons,
		persona:     definition,
		editOutcome: true,
		expiresAt:   expiresAt,
	})
	if err != nil {
		a.observeRouted(ctx, approval.RoutedUnavailable)
		return false, approvalUnavailable("owner approval card delivery failed")
	}
	a.postNotice(ctx, requester.dest, definition.Message(auraagent.MessageWaitingForOwner))
	outcome, timedOut, err := a.awaitApproval(ctx, token, &card)
	notify := context.WithoutCancel(ctx)
	switch {
	case timedOut || errors.Is(err, context.DeadlineExceeded):
		a.observeRouted(ctx, approval.RoutedTimeout)
		a.postNotice(notify, requester.dest, definition.Message(auraagent.MessageApprovalTimeout))
		if err != nil {
			return false, err
		}
		return false, approvalTimeout("owner did not answer the approval in time")
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
		a.postNotice(notify, requester.dest, definition.Message(auraagent.MessageApprovalDenied))
		return false, nil
	}
}

func (a *Adapter) openCard(ctx context.Context, token string, card *cardRequest) (openedCard, error) {
	tokenID, pending, err := a.approvals.Issue(card.binding, card.expiresAt)
	if err != nil {
		return openedCard{}, err
	}
	callbacks := a.approvals.Callbacks(tokenID, card.binding, card.expiresAt)
	buttons := card.buttons
	if buttons.Approve == "" || buttons.Reject == "" {
		buttons = approval.RenderButtons(nil, 0)
	}
	row := []inlineButton{{Text: buttons.Approve, CallbackData: callbacks.Approve}}
	if callbacks.Standing != "" {
		row = append(row, inlineButton{Text: buttons.Standing, CallbackData: callbacks.Standing})
	}
	row = append(row, inlineButton{Text: buttons.Reject, CallbackData: callbacks.Reject})
	call, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	sent, err := a.bot.sendMessage(call, token, &sendMessageRequest{
		ChatID:             card.dest.chatID,
		MessageThreadID:    card.dest.threadID,
		Text:               card.text,
		Entities:           card.entities,
		ReplyMarkup:        &inlineKeyboard{Rows: [][]inlineButton{row}},
		LinkPreviewOptions: previewsDisabled(),
	})
	if err != nil {
		a.approvals.Drop(tokenID, pending)
		return openedCard{}, cardSendError(err)
	}
	if sent.MessageID == 0 {
		a.approvals.Drop(tokenID, pending)
		return openedCard{}, deliveryAmbiguous("approval card post has no message id")
	}
	return openedCard{
		tokenID:     tokenID,
		pending:     pending,
		dest:        card.dest,
		messageID:   sent.MessageID,
		text:        card.text,
		entities:    card.entities,
		persona:     card.persona,
		editOutcome: card.editOutcome,
	}, nil
}

func cardSendError(err error) error {
	if _, rejected := apiErrorOf(err); rejected || isDialError(err) {
		return err
	}
	return deliveryAmbiguous("approval card send outcome is unknown")
}

func (a *Adapter) awaitApproval(ctx context.Context, token string, card *openedCard) (approval.Outcome, bool, error) {
	outcome, timedOut, err := a.approvals.Await(ctx, card.tokenID, card.pending)
	notify := context.WithoutCancel(ctx)
	if card.editOutcome {
		a.finishToolCard(notify, token, card, approval.CardOutcomeOf(outcome, timedOut, err))
	} else {
		a.clearKeyboard(notify, token, card)
	}
	return outcome, timedOut, err
}

func (a *Adapter) finishToolCard(ctx context.Context, token string, card *openedCard, outcome approval.CardOutcome) {
	line := approval.RenderOutcomeLine(&approval.OutcomeLineInput{
		Persona:     &card.persona,
		Outcome:     outcome,
		StandingTTL: a.access.StandingGrantTTL,
		At:          time.Now(),
		Location:    a.ownerLoc,
	})
	call, cancel := context.WithTimeout(ctx, noticeTimeout)
	defer cancel()
	_, err := a.bot.editMessageText(call, token, &editMessageTextRequest{
		ChatID:             card.dest.chatID,
		MessageID:          card.messageID,
		Text:               card.text + "\n" + line,
		Entities:           card.entities,
		ReplyMarkup:        &inlineKeyboard{Rows: [][]inlineButton{}},
		LinkPreviewOptions: previewsDisabled(),
	})
	if err != nil {
		a.logger.WarnContext(ctx, "approval card outcome edit failed", "component", "telegram", "account", a.account.Name)
		a.clearKeyboard(ctx, token, card)
	}
}

func (a *Adapter) clearKeyboard(ctx context.Context, token string, card *openedCard) {
	call, cancel := context.WithTimeout(ctx, noticeTimeout)
	defer cancel()
	if err := a.bot.clearKeyboard(call, token, card.dest.chatID, card.messageID); err != nil {
		a.logger.WarnContext(ctx, "approval keyboard removal failed", "component", "telegram", "account", a.account.Name)
	}
}

func (a *Adapter) handleCallback(ctx context.Context, query *callbackQuery) {
	resolved := a.resolveCallback(ctx, query)
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

func (a *Adapter) resolveCallback(ctx context.Context, query *callbackQuery) bool {
	if query == nil || query.ID == "" || query.From == nil || query.From.ID <= 0 || query.From.IsBot || query.Message == nil || query.Message.Chat.ID == 0 {
		return false
	}
	_, action, _, ok := approval.ParseCallbackData(query.Data)
	if !ok {
		return false
	}
	presser := &approval.Presser{
		Actor: strconv.FormatInt(query.From.ID, 10),
		Scope: scopeOf(query.Message.destination()),
	}
	if action == approval.ActionStop {
		if a.stops == nil || !a.stops.NoteSeen(query.ID) {
			return false
		}
		resolvedAction, binding, resolved := a.stops.ResolveAction(query.Data, presser)
		if !resolved || resolvedAction != approval.ActionStop {
			return false
		}
		a.cancelStoppedTurn(ctx, binding.SessionID, binding.TurnID)
		return true
	}
	if !a.approvals.NoteSeen(query.ID) {
		return false
	}
	return a.approvals.Resolve(query.Data, presser)
}

func (a *Adapter) cancelStoppedTurn(ctx context.Context, sessionID, turnID string) {
	if a.commands == nil || sessionID == "" || turnID == "" {
		return
	}
	if _, err := a.commands.CancelTurn(ctx, sessionID, turnID); err != nil {
		a.logger.WarnContext(ctx, "stop turn cancel failed", "component", "telegram", "account", a.account.Name, "error", err)
	}
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
