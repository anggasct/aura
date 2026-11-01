package discord

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode"

	"github.com/anggasct/aura/internal/command"
	"github.com/anggasct/aura/internal/effect"
	"github.com/anggasct/aura/internal/principal"
	runtimechannelhost "github.com/anggasct/aura/internal/runtime/channelhost"
	runtimeingress "github.com/anggasct/aura/internal/runtime/ingress"
)

const ProviderCommand = "command"

func CommandBinding() command.Binding {
	return command.Binding{
		Channel: principal.ChannelDiscord,
		Style:   command.StyleMessenger,
		Names:   []string{"help", "status", "session", "new", "cancel", "skill", "skill-create"},
	}
}

func (a *Adapter) SetCommands(commands *command.Dispatcher) {
	if a == nil {
		return
	}
	a.commands = commands
}

type commandRequest struct {
	Command      string `json:"command"`
	Conversation string `json:"conversation"`
	Target       string `json:"target,omitempty"`
}

type commandReceipt struct {
	Lines []string                 `json:"lines"`
	Code  string                   `json:"code,omitempty"`
	Skill *runtimeingress.SkillPin `json:"skill,omitempty"`
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
	def, _ := a.commands.Registry().Lookup(inv.Name)
	request := commandRequest{Command: def.Name, Conversation: scope.conversationKey}
	if def.Name == "session" && len(inv.Args) > 0 {
		request.Target = inv.Args[0]
	}
	raw, err := marshalOperation(request)
	if err != nil {
		return false, err
	}
	provider := &commandProvider{run: func(ctx context.Context) command.Result {
		return a.commands.Execute(ctx, caller, inv)
	}}
	intent, err := a.effects.Execute(ctx, &effect.PrepareRequest{
		SessionID:      scope.effectSession,
		IdempotencyKey: envelope.Source + ":" + envelope.ExternalID,
		Provider:       ProviderCommand,
		Operation:      def.Name,
		Classification: effect.ClassificationEffectful,
		Request:        raw,
		EventKind:      effect.EventKindChannelRequested,
	}, provider)
	if err != nil {
		return false, fmt.Errorf("command effect: %w", err)
	}
	result := command.ResultReplayed
	if provider.ran {
		result = command.ResultOf(command.ErrorCode(provider.code))
	} else {
		a.commands.Replayed(ctx, caller, inv)
	}
	a.logCommand(ctx, caller, a.commands.Registry().MetricName(principal.ChannelDiscord, inv.Name), result, scope.effectSession)
	if intent == nil || intent.State != effect.StateSucceeded {
		return true, nil
	}
	receipt, ok := decodeReceipt(intent.ProviderReceipt)
	if !ok {
		a.logger.WarnContext(ctx, "command receipt unreadable", "component", "command", "session_id", scope.effectSession)
		return true, nil
	}
	if receipt.Skill != nil {
		envelope.Skill = receipt.Skill
		return false, nil
	}
	if err := a.replyCommand(ctx, msg, intent.ID, scope.effectSession, receipt.Lines); err != nil {
		a.logger.WarnContext(ctx, "command reply failed", "component", "command", "session_id", scope.effectSession, "error", err)
	}
	return true, nil
}

func decodeReceipt(raw json.RawMessage) (commandReceipt, bool) {
	var receipt commandReceipt
	if json.Unmarshal(raw, &receipt) != nil || (len(receipt.Lines) == 0 && receipt.Skill == nil) {
		return commandReceipt{}, false
	}
	return receipt, true
}

func (a *Adapter) logCommand(ctx context.Context, caller *command.Caller, name, result, sessionID string) {
	attrs := []any{
		"component", "command",
		"name", name,
		"channel", caller.Channel,
		"class", string(caller.Class),
		"result", result,
		"session_id", sessionID,
	}
	switch result {
	case command.ResultDenied, command.ResultUnavailable, command.ResultFailed:
		a.logger.WarnContext(ctx, "chat command", attrs...)
	default:
		a.logger.InfoContext(ctx, "chat command", attrs...)
	}
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

type commandProvider struct {
	run  func(ctx context.Context) command.Result
	ran  bool
	code string
}

func (p *commandProvider) SupportsIdempotency() bool { return false }

func (p *commandProvider) Invoke(ctx context.Context, _ *effect.Invocation) (effect.Outcome, error) {
	result := p.run(ctx)
	p.ran = true
	p.code = string(result.Code)
	var skill *runtimeingress.SkillPin
	if result.Turn != nil {
		skill = result.Turn.Skill
	}
	receipt, err := json.Marshal(commandReceipt{Lines: result.Lines, Code: p.code, Skill: skill})
	if err != nil {
		return effect.Outcome{}, Errorf(ErrorCodeProtocolInvalid, "command receipt is not serializable")
	}
	return effect.Outcome{Succeeded: true, Receipt: receipt}, nil
}
