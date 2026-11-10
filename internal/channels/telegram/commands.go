package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/anggasct/aura/internal/channels/kit/chatcommand"
	"github.com/anggasct/aura/internal/platform/command"
	"github.com/anggasct/aura/internal/platform/principal"
	runtimechannelhost "github.com/anggasct/aura/internal/platform/turn/channelhost"
	runtimeingress "github.com/anggasct/aura/internal/platform/turn/ingress"
)

func CommandBinding() command.Binding {
	return command.Binding{
		Channel: principal.ChannelTelegram,
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

func (a *Adapter) commandInvocation(text string) (command.Invocation, bool) {
	if self, known := a.selfIdentity(); known {
		text = stripCommandSuffix(text, self.Username)
	}
	inv, ok := command.Parse(text)
	if !ok {
		return command.Invocation{}, false
	}
	if inv.Name == command.NameForgetMe {
		return inv, a.access.Erasure != nil
	}
	if a.commands == nil || !a.commands.Registry().Recognized(principal.ChannelTelegram, inv.Name) {
		return command.Invocation{}, false
	}
	return inv, true
}

func stripCommandSuffix(text, username string) string {
	fields := strings.Fields(text)
	if len(fields) == 0 || username == "" || !strings.HasPrefix(fields[0], "/") {
		return text
	}
	head, target, found := strings.Cut(fields[0], "@")
	if !found || !strings.EqualFold(target, username) {
		return text
	}
	start := strings.Index(text, fields[0])
	return text[:start] + head + text[start+len(fields[0]):]
}

func (a *Adapter) runCommand(ctx context.Context, msg *message, inv *command.Invocation, envelope *runtimeingress.IngressEnvelope) (bool, error) {
	if inv.Name == command.NameForgetMe {
		a.forgetMe(ctx, msg.destination(), inv, envelope)
		return true, nil
	}
	conversation := envelope.ConversationID
	active, err := a.activeSession(ctx, conversation)
	if err != nil {
		return false, &Error{Code: ErrorCodeIntakeFailed, Detail: "conversation session is not resolvable", Cause: err}
	}
	caller := &command.Caller{
		Channel:         principal.ChannelTelegram,
		PrincipalID:     envelope.PrincipalID,
		Class:           principal.ParseClass(envelope.PrincipalClass),
		ConversationKey: conversation,
		SessionID:       active,
		AgentID:         envelope.AgentID,
	}
	if err := a.sessions.EnsureSession(ctx, active, envelope.PrincipalID, envelope.AgentID); err != nil {
		return false, &Error{Code: ErrorCodeIntakeFailed, Detail: "session is not ensurable", Cause: err}
	}
	outcome, err := chatcommand.Execute(ctx, &chatcommand.Run{
		Effects:        a.effects,
		Dispatcher:     a.commands,
		Logger:         a.logger,
		Caller:         caller,
		Invocation:     inv,
		IdempotencyKey: envelope.Source + ":" + envelope.ExternalID,
		EffectSession:  active,
	})
	if err != nil {
		var rejected *chatcommand.Error
		if errors.As(err, &rejected) {
			a.logger.WarnContext(ctx, "chat command rejected", "component", "command", "account", a.account.Name, "code", string(rejected.Code))
			return true, nil
		}
		return false, &Error{Code: ErrorCodeIntakeFailed, Detail: "command could not run", Cause: err}
	}
	if outcome.Receipt == nil {
		return true, nil
	}
	if outcome.Receipt.Skill != nil {
		envelope.Skill = outcome.Receipt.Skill
		return false, nil
	}
	if err := a.replyCommand(ctx, msg, outcome.IntentID, active, outcome.Receipt.Lines); err != nil {
		a.logger.WarnContext(ctx, "command reply failed", "component", "command", "account", a.account.Name, "session_id", active, "error", err)
	}
	return true, nil
}

func (a *Adapter) replyCommand(ctx context.Context, msg *message, intentID, sessionID string, lines []string) error {
	reference, err := json.Marshal(replyReference{ChatID: msg.Chat.ID, MessageID: msg.MessageID, ThreadID: msg.topicID()})
	if err != nil {
		return Errorf(ErrorCodeProtocolInvalid, "reply reference is not serializable")
	}
	_, err = a.Deliver(ctx, &runtimechannelhost.DeliveryRequest{
		Channel:        a.source(),
		ConversationID: sessionID,
		EffectID:       intentID,
		ReplyContext:   reference,
		Parts:          []runtimechannelhost.OutputPart{{Text: strings.Join(lines, "\n")}},
	})
	if err != nil {
		return fmt.Errorf("command reply: %w", err)
	}
	return nil
}
