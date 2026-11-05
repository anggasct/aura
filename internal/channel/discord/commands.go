package discord

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode"

	"github.com/anggasct/aura/internal/channel/chatcommand"
	"github.com/anggasct/aura/internal/command"
	"github.com/anggasct/aura/internal/principal"
	runtimechannelhost "github.com/anggasct/aura/internal/runtime/channelhost"
	runtimeingress "github.com/anggasct/aura/internal/runtime/ingress"
)

func CommandBinding() command.Binding {
	return command.Binding{
		Channel: principal.ChannelDiscord,
		Style:   command.StyleMessenger,
		Names:   command.MessengerNames(),
	}
}

func (a *Adapter) SetCommands(commands *command.Dispatcher) {
	if a == nil {
		return
	}
	a.commands = commands
}

type commandScope struct {
	conversationKey string
	sessionID       string
	effectSession   string
}

func (a *Adapter) commandInvocation(msg *messagePayload) (command.Invocation, bool) {
	if a.commands == nil || msg == nil {
		return command.Invocation{}, false
	}
	inv, ok := command.Parse(stripLeadingMentions(msg.Content, a.selfID))
	if !ok || !a.commands.Registry().Recognized(principal.ChannelDiscord, inv.Name) {
		return command.Invocation{}, false
	}
	return inv, true
}

func stripLeadingMentions(text, selfID string) string {
	rest := strings.TrimLeftFunc(text, unicode.IsSpace)
	if selfID == "" {
		return rest
	}
	mentions := []string{"<@" + selfID + ">", "<@!" + selfID + ">"}
	for {
		stripped := false
		for _, mention := range mentions {
			if strings.HasPrefix(rest, mention) {
				rest = strings.TrimLeftFunc(rest[len(mention):], unicode.IsSpace)
				stripped = true
			}
		}
		if !stripped {
			return rest
		}
	}
}

func (a *Adapter) conversationScope(ctx context.Context, envelope *runtimeingress.IngressEnvelope, msg *messagePayload) (commandScope, error) {
	key := a.sessionKey(envelope.AgentID, envelope.ConversationID)
	scope := commandScope{conversationKey: key, effectSession: key}
	if msg.GuildID != "" && !strings.HasPrefix(envelope.ConversationID, "thread:") {
		return scope, nil
	}
	active, err := a.activeSession(ctx, key)
	if err != nil {
		return commandScope{}, err
	}
	scope.sessionID, scope.effectSession = active, active
	return scope, nil
}

func (a *Adapter) runCommand(ctx context.Context, msg *messagePayload, inv *command.Invocation, envelope *runtimeingress.IngressEnvelope) (bool, error) {
	if inv.Name == command.NameForgetMe {
		a.forgetMe(ctx, msg, inv, envelope)
		return true, nil
	}
	scope, err := a.conversationScope(ctx, envelope, msg)
	if err != nil {
		return false, err
	}
	caller := &command.Caller{
		Channel:         principal.ChannelDiscord,
		PrincipalID:     envelope.PrincipalID,
		Class:           principal.ParseClass(envelope.PrincipalClass),
		ConversationKey: scope.conversationKey,
		SessionID:       scope.sessionID,
		AgentID:         envelope.AgentID,
	}
	if err := a.sessions.EnsureSession(ctx, scope.effectSession, envelope.PrincipalID, envelope.AgentID); err != nil {
		return false, fmt.Errorf("ensure session: %w", err)
	}
	outcome, err := chatcommand.Execute(ctx, &chatcommand.Run{
		Effects:        a.effects,
		Dispatcher:     a.commands,
		Logger:         a.logger,
		Caller:         caller,
		Invocation:     inv,
		IdempotencyKey: envelope.Source + ":" + envelope.ExternalID,
		EffectSession:  scope.effectSession,
	})
	if err != nil {
		return false, err
	}
	if outcome.Receipt == nil {
		return true, nil
	}
	if outcome.Receipt.Skill != nil {
		envelope.Skill = outcome.Receipt.Skill
		return false, nil
	}
	if err := a.replyCommand(ctx, msg, outcome.IntentID, scope.effectSession, outcome.Receipt.Lines); err != nil {
		a.logger.WarnContext(ctx, "command reply failed", "component", "command", "session_id", scope.effectSession, "error", err)
	}
	return true, nil
}

func (a *Adapter) replyCommand(ctx context.Context, msg *messagePayload, intentID, sessionID string, lines []string) error {
	chunks := splitMessage(neutralizeMentions(strings.Join(lines, "\n")))
	texts, uploads, err := splitUploads(chunks, a.cfg.MaxAttachmentBytes)
	if err != nil {
		return err
	}
	reference, err := json.Marshal(replyReference{ChannelID: msg.ChannelID, MessageID: msg.ID, GuildID: msg.GuildID})
	if err != nil {
		return Errorf(ErrorCodeProtocolInvalid, "reply reference is not serializable")
	}
	_, err = a.postChunks(ctx, &runtimechannelhost.DeliveryRequest{
		Channel:        a.source(),
		ConversationID: sessionID,
		EffectID:       intentID,
		ReplyContext:   reference,
	}, msg.ChannelID, reference, texts, uploads)
	return err
}
