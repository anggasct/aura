package telegram

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"time"

	auraagent "github.com/anggasct/aura/internal/agent"
	"github.com/anggasct/aura/internal/channel/access"
	"github.com/anggasct/aura/internal/channel/status"
	"github.com/anggasct/aura/internal/command"
	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/principal"
	runtimeingress "github.com/anggasct/aura/internal/runtime/ingress"
)

const (
	noticeTimeout       = 15 * time.Second
	handoffForwardRunes = 1500
)

type AccessSettings struct {
	Pairing  access.PairingService
	Budgets  access.BudgetGate
	Erasure  access.PrincipalEraser
	Handoffs access.HandoffGate
	Evals    access.EvalGate
}

func (a *Adapter) SetAccess(settings *AccessSettings) {
	if a == nil || settings == nil {
		return
	}
	a.access = *settings
}

type authorAccess struct {
	servesPublic bool
	granted      func(userID string) bool
}

func (a *Adapter) authorAccess(ctx context.Context) authorAccess {
	result := authorAccess{servesPublic: a.router != nil && a.router.ServesPublic(config.BindingChannelTelegram, a.account.Name)}
	if a.access.Pairing == nil {
		return result
	}
	granted := map[string]bool{}
	result.granted = func(userID string) bool {
		if cached, ok := granted[userID]; ok {
			return cached
		}
		active, err := a.access.Pairing.Granted(ctx, principal.Namespaced(principal.ChannelTelegram, userID), a.account.Name)
		if err != nil {
			a.logger.WarnContext(ctx, "principal grant lookup failed", "component", "telegram")
			active = false
		}
		granted[userID] = active
		return active
	}
	return result
}

func (a *Adapter) resolveAuthor(userID string, reach authorAccess) (string, principal.Class, bool) {
	allowlisted := slices.Contains(a.account.AllowedUserIDs, userID)
	if !allowlisted && reach.granted != nil && userID != "" && !a.owners.IsOwnerIdentity(principal.ChannelTelegram, userID) {
		allowlisted = reach.granted(userID)
	}
	return a.owners.Resolve(principal.ChannelTelegram, userID, allowlisted, reach.servesPublic)
}

func (a *Adapter) personaDefinition(persona string) (auraagent.Definition, bool) {
	if a.router == nil {
		return auraagent.Definition{}, false
	}
	return a.router.Persona(persona)
}

func (a *Adapter) personaAdmits(ctx context.Context, persona string, class principal.Class) bool {
	definition, ok := a.personaDefinition(persona)
	if ok && definition.Admits(class) {
		return true
	}
	a.deny(ctx, access.DeniedReasonAudience)
	return false
}

func (a *Adapter) personaEvaluated(ctx context.Context, chatID int64, persona string, class principal.Class) bool {
	if class == principal.ClassOwner || a.access.Evals == nil {
		return true
	}
	definition, ok := a.personaDefinition(persona)
	if ok && definition.Audience == config.AudienceOwner {
		return true
	}
	evaluated, err := a.access.Evals.Evaluated(ctx, persona)
	if err != nil {
		a.logger.WarnContext(ctx, "persona eval lookup failed", "component", "telegram")
	}
	if err == nil && evaluated {
		return true
	}
	a.deny(ctx, access.DeniedReasonEvalMissing)
	a.postNotice(ctx, chatID, definition.Message(config.MessageUnavailable))
	return false
}

func (a *Adapter) deny(ctx context.Context, reason string) {
	if a.denied != nil {
		a.denied(ctx, reason)
	}
}

func (a *Adapter) tryPairing(ctx context.Context, msg *message, reach authorAccess) bool {
	if a.access.Pairing == nil || msg.Chat.Type != chatTypePrivate || !a.account.AcceptPrivateChats {
		return false
	}
	if msg.From == nil || msg.From.IsBot || msg.From.ID <= 0 || strings.TrimSpace(msg.Text) == "" {
		return false
	}
	userID := strconv.FormatInt(msg.From.ID, 10)
	if _, class, admitted := a.resolveAuthor(userID, reach); admitted && class != principal.ClassPublic {
		return false
	}
	persona, routed := a.routePersona(userID)
	if !routed {
		return false
	}
	definition, ok := a.personaDefinition(persona)
	if !ok || !definition.Pairing || definition.Audience != config.AudienceTrusted {
		return false
	}
	result, err := a.access.Pairing.Request(ctx, principal.Namespaced(principal.ChannelTelegram, userID), a.account.Name)
	if err != nil {
		a.logger.WarnContext(ctx, "pairing request failed", "component", "telegram")
		return true
	}
	var reply string
	switch result.Outcome {
	case access.PairingOutcomeIssued:
		reply = strings.ReplaceAll(definition.Message(config.MessagePairingCode), config.PairingCodePlaceholder, result.Code)
	case access.PairingOutcomeLocked:
		reply = definition.Message(config.MessagePairingLocked)
	default:
		reply = definition.Message(config.MessagePairingPending)
	}
	a.postNotice(ctx, msg.Chat.ID, reply)
	return true
}

func (a *Adapter) budgetDrop(ctx context.Context, chatID int64, envelope *runtimeingress.IngressEnvelope) dropReason {
	class := principal.ParseClass(envelope.PrincipalClass)
	if a.access.Budgets == nil || class == principal.ClassOwner {
		return ""
	}
	refusal, err := a.access.Budgets.Admit(ctx, envelope.PrincipalID, class)
	if err != nil {
		a.logger.WarnContext(ctx, "principal budget check failed", "component", "telegram")
		return dropBudgetCheckFailed
	}
	if refusal == "" {
		return ""
	}
	reason := dropReason(access.BudgetDropReason(refusal))
	definition, _ := a.personaDefinition(envelope.AgentID)
	a.postNotice(ctx, chatID, definition.Message(refusal))
	return reason
}

func (a *Adapter) forgetMeInvocation(text string) (command.Invocation, bool) {
	if a.access.Erasure == nil {
		return command.Invocation{}, false
	}
	inv, ok := command.Parse(text)
	if !ok || inv.Name != command.NameForgetMe {
		return command.Invocation{}, false
	}
	return inv, true
}

func (a *Adapter) forgetMe(ctx context.Context, chatID int64, inv *command.Invocation, envelope *runtimeingress.IngressEnvelope) {
	definition, _ := a.personaDefinition(envelope.AgentID)
	if principal.ParseClass(envelope.PrincipalClass) == principal.ClassOwner {
		a.postNotice(ctx, chatID, definition.Message(config.MessageNotPermitted))
		return
	}
	if len(inv.Args) != 1 || !strings.EqualFold(inv.Args[0], command.ArgConfirm) {
		a.postNotice(ctx, chatID, definition.Message(config.MessageForgetmeConfirm))
		return
	}
	if err := a.access.Erasure.Forget(ctx, envelope.PrincipalID); err != nil {
		a.logger.WarnContext(ctx, "forgetme erasure failed", "component", "telegram")
		return
	}
	a.postNotice(ctx, chatID, definition.Message(config.MessageForgetmeDone))
}

func (a *Adapter) handoffHeld(ctx context.Context, chatID int64, text string, envelope *runtimeingress.IngressEnvelope) (held bool, reason dropReason) {
	if a.access.Handoffs == nil || principal.ParseClass(envelope.PrincipalClass) == principal.ClassOwner {
		return false, ""
	}
	state, err := a.access.Handoffs.Check(ctx, envelope.ConversationID)
	if err != nil {
		a.logger.WarnContext(ctx, "handoff check failed", "component", "telegram")
		return true, dropHandoffCheckFailed
	}
	switch state {
	case access.HandoffStateOpen:
		ownerChat, ok := a.ownerChatID()
		if !ok {
			a.logger.WarnContext(ctx, "handoff forward failed", "component", "telegram")
			return true, dropHandoffForwardFailed
		}
		forward := "handoff · session " + envelope.ConversationID + "\n" + envelope.PrincipalID + ": " + status.TruncateRunes(strings.TrimSpace(text), handoffForwardRunes)
		a.postNotice(ctx, ownerChat, forward)
		return true, ""
	case access.HandoffStateExpired:
		definition, _ := a.personaDefinition(envelope.AgentID)
		a.postNotice(ctx, chatID, definition.Message(config.MessageHandoffClosed))
		return false, ""
	default:
		return false, ""
	}
}

func (a *Adapter) ownerChatID() (int64, bool) {
	for _, id := range a.owners.OwnerExternalIDs(principal.ChannelTelegram) {
		if chatID, err := strconv.ParseInt(id, 10, 64); err == nil && chatID > 0 {
			return chatID, true
		}
	}
	return 0, false
}

func (a *Adapter) postNotice(ctx context.Context, chatID int64, text string) {
	if chatID == 0 || strings.TrimSpace(text) == "" {
		return
	}
	token, ok := a.botToken()
	if !ok {
		a.logger.WarnContext(ctx, "access notice token unavailable", "component", "telegram")
		return
	}
	chunks := splitMessage(text)
	if len(chunks) == 0 {
		return
	}
	call, cancel := context.WithTimeout(ctx, noticeTimeout)
	defer cancel()
	if _, err := a.bot.sendMessage(call, token, &sendMessageRequest{ChatID: chatID, Text: chunks[0]}); err != nil {
		a.logger.WarnContext(ctx, "access notice delivery failed", "component", "telegram")
	}
}
