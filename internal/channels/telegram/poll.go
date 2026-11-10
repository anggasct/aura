package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/anggasct/aura/internal/channels/kit/approval"
	"github.com/anggasct/aura/internal/channels/kit/media"
	"github.com/anggasct/aura/internal/config"
	auraagent "github.com/anggasct/aura/internal/platform/agent"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/turn"
	runtimeingress "github.com/anggasct/aura/internal/platform/turn/ingress"
)

const pollGrace = 10 * time.Second

const chatTypePrivate = "private"

type replyReference struct {
	ChatID    int64                   `json:"chat_id"`
	MessageID int64                   `json:"message_id"`
	ThreadID  int64                   `json:"message_thread_id,omitempty"`
	Artifacts []media.ArtifactSummary `json:"artifacts,omitempty"`
}

func (a *Adapter) pollLoop(ctx context.Context) {
	attempt := 0
	for {
		err := a.pollOnce(ctx)
		select {
		case <-ctx.Done():
			return
		default:
		}
		if err == nil {
			if attempt > 0 {
				a.refreshIdentity()
			}
			attempt = 0
			continue
		}
		a.markFault(classifyFault(err))
		a.logger.WarnContext(ctx, "telegram poll failed", "component", "telegram", "account", a.account.Name, "error", err)
		if !sleepContext(ctx, a.retryDelay(attempt, err)) {
			return
		}
		attempt++
	}
}

func (a *Adapter) pollOnce(ctx context.Context) error {
	token, ok := a.botToken()
	if !ok {
		return errTokenUnavailable
	}
	next, _, err := a.cursors.Load(ctx, a.source())
	if err != nil {
		return &Error{Code: ErrorCodeIntakeFailed, Detail: "telegram cursor is not readable", Cause: err}
	}
	timeout := time.Duration(a.cfg.Polling.Timeout)
	call, cancel := context.WithTimeout(ctx, timeout+pollGrace)
	updates, err := a.bot.getUpdates(call, token, next, timeout)
	cancel()
	if err != nil {
		return err
	}
	a.markLive()
	advance := next
	for i := range updates {
		item := &updates[i]
		if item.UpdateID < next {
			continue
		}
		if err := a.handleUpdate(ctx, item); err != nil {
			if advance > next {
				a.advanceCursor(ctx, advance)
			}
			return err
		}
		if item.UpdateID+1 > advance {
			advance = item.UpdateID + 1
		}
	}
	if advance > next {
		return a.advanceCursorErr(ctx, advance)
	}
	return nil
}

func (a *Adapter) advanceCursor(ctx context.Context, next int64) {
	if err := a.advanceCursorErr(ctx, next); err != nil {
		a.logger.WarnContext(ctx, "telegram cursor save failed", "component", "telegram", "account", a.account.Name)
	}
}

func (a *Adapter) advanceCursorErr(ctx context.Context, next int64) error {
	if err := a.cursors.Advance(ctx, a.source(), next, time.Now().UTC()); err != nil {
		return &Error{Code: ErrorCodeIntakeFailed, Detail: "telegram cursor is not writable", Cause: err}
	}
	return nil
}

func (a *Adapter) handleUpdate(ctx context.Context, item *update) error {
	if item.CallbackQuery != nil {
		a.handleCallback(ctx, item.CallbackQuery)
		return nil
	}
	envelope, msg, ok := a.admit(ctx, item)
	if !ok {
		return nil
	}
	if inv, isCommand := a.commandInvocation(envelope.Parts[0].Text); isCommand {
		handled, err := a.runCommand(ctx, msg, &inv, envelope)
		if err != nil {
			return err
		}
		if handled {
			return nil
		}
	}
	if reason := a.budgetDrop(ctx, msg.destination(), envelope); reason != "" {
		a.drop(ctx, messageScope(msg), reason, true)
		return nil
	}
	active, err := a.activeSession(ctx, envelope.ConversationID)
	if err != nil {
		return &Error{Code: ErrorCodeIntakeFailed, Detail: "conversation session is not resolvable", Cause: err}
	}
	envelope.ConversationID = active
	if held, reason := a.handoffHeld(ctx, msg.destination(), envelope.Parts[0].Text, envelope); held {
		if reason != "" {
			a.drop(ctx, messageScope(msg), reason, true)
		}
		return nil
	}
	a.rememberTarget(envelope.ConversationID, &approvalTarget{
		binding: approval.Binding{
			Principal: envelope.PrincipalID,
			Class:     principal.ParseClass(envelope.PrincipalClass),
			Persona:   envelope.AgentID,
			Actor:     strconv.FormatInt(msg.From.ID, 10),
			Scope:     scopeOf(msg.destination()),
		},
		dest: msg.destination(),
	})
	if err := a.sessions.EnsureSession(ctx, envelope.ConversationID, envelope.PrincipalID, envelope.AgentID); err != nil {
		return &Error{Code: ErrorCodeIntakeFailed, Detail: "session is not ensurable", Cause: err}
	}
	if msg.hasMedia() {
		proceed, err := a.stageMedia(ctx, msg, envelope)
		if err != nil || !proceed {
			return err
		}
	}
	if _, err := a.sink.Accept(ctx, envelope); err != nil {
		if retryableIntake(err) {
			return &Error{Code: ErrorCodeIntakeFailed, Detail: "runtime did not accept the update", Cause: err}
		}
		a.logger.WarnContext(ctx, "telegram update rejected by the runtime", "component", "telegram", "account", a.account.Name, "error", err)
		return nil
	}
	if a.admitted != nil {
		a.admitted(ctx, principal.ParseClass(envelope.PrincipalClass))
	}
	return nil
}

func (a *Adapter) admit(ctx context.Context, item *update) (*runtimeingress.IngressEnvelope, *message, bool) {
	msg := item.Message
	if msg == nil {
		return nil, nil, false
	}
	scope := messageScope(msg)
	switch {
	case msg.From == nil || msg.From.ID <= 0:
		a.drop(ctx, scope, dropAuthorMissing, false)
		return nil, nil, false
	case msg.From.IsBot:
		a.drop(ctx, scope, dropAuthorBot, false)
		return nil, nil, false
	}
	if reason := a.chatDrop(msg); reason != "" {
		a.drop(ctx, scope, reason, false)
		return nil, nil, false
	}
	if !msg.hasContent() {
		a.drop(ctx, scope, dropEmptyContent, false)
		return nil, nil, false
	}
	text := msg.body()
	if msg.isGroup() {
		var reason dropReason
		if text, reason = a.groupText(msg); reason != "" {
			a.drop(ctx, scope, reason, false)
			return nil, nil, false
		}
	}
	userID := strconv.FormatInt(msg.From.ID, 10)
	reach := a.authorAccess(ctx)
	principalID, class, known := a.resolveAuthor(userID, reach)
	if !known {
		if a.tryPairing(ctx, msg, reach) {
			a.drop(ctx, scope, dropPairing, false)
			return nil, nil, false
		}
		a.deny(ctx, DeniedReasonIdentity)
		a.drop(ctx, scope, dropAuthorNotAllowed, false)
		return nil, nil, false
	}
	if !msg.isGroup() && class != principal.ClassOwner && !a.account.AcceptPrivateChats {
		a.drop(ctx, scope, dropPrivateChatsDisabled, true)
		return nil, nil, false
	}
	persona, routed := a.routePersona(userID, msg)
	if !routed {
		a.logger.DebugContext(ctx, "persona unresolved", "component", "telegram", "account", a.account.Name)
		a.drop(ctx, scope, dropPersonaUnrouted, true)
		return nil, nil, false
	}
	if !a.personaAdmits(ctx, persona, class) {
		if a.tryPairing(ctx, msg, reach) {
			a.drop(ctx, scope, dropPairing, true)
			return nil, nil, false
		}
		a.drop(ctx, scope, dropAudience, true)
		return nil, nil, false
	}
	if !a.personaEvaluated(ctx, msg.destination(), persona, class) {
		a.drop(ctx, scope, dropPersonaEvalMissing, true)
		return nil, nil, false
	}
	reference, err := json.Marshal(replyReference{ChatID: msg.Chat.ID, MessageID: msg.MessageID, ThreadID: msg.topicID()})
	if err != nil {
		return nil, nil, false
	}
	conversation := "chat:" + strconv.FormatInt(msg.Chat.ID, 10)
	if msg.isGroup() {
		conversation = a.groupConversation(persona, msg, principalID, class)
	}
	envelope := &runtimeingress.IngressEnvelope{
		Source:         a.source(),
		AgentID:        persona,
		ExternalID:     "update:" + strconv.FormatInt(item.UpdateID, 10),
		PrincipalID:    principalID,
		PrincipalClass: string(class),
		ConversationID: runtimeingress.SessionKey(persona, a.source(), conversation),
		ReplyContext:   reference,
		Parts:          []runtimeingress.InputPart{{Text: text}},
		ReceivedAt:     time.Now().UTC(),
	}
	return envelope, msg, true
}

func (a *Adapter) chatDrop(msg *message) dropReason {
	switch {
	case msg.Chat.Type == chatTypePrivate:
		return ""
	case !msg.isGroup() || len(a.account.AllowedChatIDs) == 0:
		return dropChatNotSupported
	case !a.chatAllowed(msg.Chat.ID):
		return dropChatNotAllowed
	default:
		return ""
	}
}

func (a *Adapter) groupText(msg *message) (string, dropReason) {
	self, known := a.selfIdentity()
	if !known {
		return "", dropBotIdentityUnknown
	}
	text, addressed := a.groupAddressing(msg, self)
	switch {
	case !addressed:
		return "", dropNotMentioned
	case strings.TrimSpace(text) == "" && !msg.hasMedia():
		return "", dropEmptyContent
	default:
		return text, ""
	}
}

func (a *Adapter) routePersona(userID string, msg *message) (string, bool) {
	if a.router == nil {
		return "", false
	}
	scope := &auraagent.RouteScope{
		Channel: config.BindingChannelTelegram,
		Account: a.account.Name,
		UserID:  userID,
	}
	if msg.isGroup() {
		scope.ChatID = strconv.FormatInt(msg.Chat.ID, 10)
		if topic := msg.topicID(); topic > 0 {
			scope.ThreadID = strconv.FormatInt(topic, 10)
		}
	}
	match, ok := a.router.Resolve(scope)
	if !ok {
		return "", false
	}
	return match.Persona, true
}

func (a *Adapter) activeSession(ctx context.Context, conversationKey string) (string, error) {
	if a.resolver == nil {
		return conversationKey, nil
	}
	active, err := a.resolver.ActiveSession(ctx, conversationKey)
	if err != nil {
		return "", fmt.Errorf("resolve conversation session: %w", err)
	}
	if active == "" {
		return conversationKey, nil
	}
	return active, nil
}

func retryableIntake(err error) bool {
	code, ok := turn.CodeOf(err)
	if !ok {
		return true
	}
	switch code {
	case turn.ErrorCodeRuntimeOverloaded, turn.ErrorCodeStorageUnavailable, turn.ErrorCodeRuntimeInternal:
		return true
	default:
		return false
	}
}

var errTokenUnavailable = Errorf(ErrorCodeConnectionFailed, "bot token is unavailable")

func classifyFault(err error) string {
	if errors.Is(err, errTokenUnavailable) {
		return faultBotRefUnresolved
	}
	if errors.Is(err, errWebhookSecretInvalid) {
		return faultWebhookSecretInvalid
	}
	if apiErr, ok := apiErrorOf(err); ok {
		switch apiErr.Status {
		case http.StatusUnauthorized, http.StatusNotFound:
			return faultUnauthorized
		case http.StatusConflict:
			return faultPollConflict
		}
		return faultUnreachable
	}
	if code, ok := CodeOf(err); ok && code == ErrorCodeIntakeFailed {
		return faultIntake
	}
	return faultUnreachable
}

func (a *Adapter) retryDelay(attempt int, err error) time.Duration {
	ceiling := time.Duration(a.cfg.Polling.MaxBackoff)
	if apiErr, ok := apiErrorOf(err); ok && apiErr.RetryAfter > 0 {
		if apiErr.RetryAfter > ceiling {
			return ceiling
		}
		return apiErr.RetryAfter
	}
	shift := min(attempt, 6)
	limit := a.retryBase << shift
	if limit > ceiling || limit <= 0 {
		limit = ceiling
	}
	half := int64(limit / 2)
	return time.Duration(half + rand.Int64N(half+1))
}

func sleepContext(ctx context.Context, delay time.Duration) bool {
	if delay <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
