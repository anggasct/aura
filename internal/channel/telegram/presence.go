package telegram

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/anggasct/aura/internal/channel/status"
	"github.com/anggasct/aura/internal/effect"
	runtimechannelhost "github.com/anggasct/aura/internal/runtime/channelhost"
	"github.com/anggasct/aura/internal/secret"
)

const (
	typingRefreshInterval = 4 * time.Second
	statusPostAfter       = 10 * time.Second
	typingTimeout         = 10 * time.Second
	chatActionTyping      = "typing"
	maxPresenceSessions   = 256
)

var _ runtimechannelhost.PresencePort = (*Adapter)(nil)

type presenceSession struct {
	chatID         int64
	threadID       int64
	request        *runtimechannelhost.DeliveryRequest
	started        time.Time
	lastTyping     time.Time
	statusID       int64
	lastText       string
	shownText      string
	lastEdit       time.Time
	updates        int
	attempts       int
	failedNotified bool
	statusMu       sync.Mutex
	wake           chan struct{}
	stop           chan struct{}
	done           chan struct{}
	stopped        bool
}

type presenceSnapshot struct {
	statusID   int64
	lastText   string
	shownText  string
	lastEdit   time.Time
	lastTyping time.Time
	updates    int
}

func (a *Adapter) SetSecretVault(vault *secret.Vault) {
	if a == nil {
		return
	}
	a.secretVault = vault
}

func (a *Adapter) snapshot(session *presenceSession) presenceSnapshot {
	a.presenceMu.Lock()
	defer a.presenceMu.Unlock()
	return presenceSnapshot{
		statusID:   session.statusID,
		lastText:   session.lastText,
		shownText:  session.shownText,
		lastEdit:   session.lastEdit,
		lastTyping: session.lastTyping,
		updates:    session.updates,
	}
}

func (a *Adapter) ensurePresence(req *runtimechannelhost.DeliveryRequest) (*presenceSession, error) {
	if req == nil {
		return nil, Errorf(ErrorCodeInvalidArgument, "presence request must not be nil")
	}
	if req.EffectID == "" {
		return nil, Errorf(ErrorCodeInvalidArgument, "presence request carries no turn id")
	}
	var target replyReference
	if err := json.Unmarshal(req.ReplyContext, &target); err != nil || target.ChatID == 0 {
		return nil, Errorf(ErrorCodeInvalidArgument, "presence reply context carries no chat")
	}
	a.presenceMu.Lock()
	defer a.presenceMu.Unlock()
	if session, ok := a.presence[req.EffectID]; ok {
		return session, nil
	}
	if len(a.presence) >= maxPresenceSessions {
		return nil, Errorf(ErrorCodeDeliveryUnavailable, "presence session capacity is exhausted")
	}
	cloned := *req
	cloned.Parts = nil
	cloned.ReplyContext = slices.Clone(req.ReplyContext)
	session := &presenceSession{
		chatID:   target.ChatID,
		threadID: target.ThreadID,
		request:  &cloned,
		started:  time.Now(),
		wake:     make(chan struct{}, 1),
	}
	a.presence[req.EffectID] = session
	return session, nil
}

func (a *Adapter) PresenceStarted(ctx context.Context, req *runtimechannelhost.DeliveryRequest) error {
	session, err := a.ensurePresence(req)
	if err != nil {
		return err
	}
	a.presenceMu.Lock()
	session.lastTyping = time.Now()
	startClock := session.stop == nil && !session.stopped
	if startClock {
		session.stop = make(chan struct{})
		session.done = make(chan struct{})
	}
	stop, done := session.stop, session.done
	a.presenceMu.Unlock()
	if startClock {
		go a.runPresenceClock(ctx, session, stop, done)
	}
	a.sendTyping(ctx, session.destination())
	return nil
}

func (a *Adapter) runPresenceClock(ctx context.Context, session *presenceSession, stop <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	statusTimer := time.NewTimer(max(a.statusDelay()-time.Since(session.started), 0))
	defer statusTimer.Stop()
	typingTicker := time.NewTicker(a.typingInterval())
	defer typingTicker.Stop()
	flushTimer := time.NewTimer(a.typingInterval())
	flushTimer.Stop()
	defer flushTimer.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ctx.Done():
			return
		case <-typingTicker.C:
			a.refreshPresence(ctx, session)
		case <-statusTimer.C:
			a.postInitialStatus(ctx, session)
		case <-session.wake:
			flushTimer.Reset(a.statusWait(session))
		case <-flushTimer.C:
			a.flushStatus(ctx, session)
		}
	}
}

func (a *Adapter) refreshPresence(ctx context.Context, session *presenceSession) {
	current := a.snapshot(session)
	if current.statusID != 0 {
		return
	}
	now := time.Now()
	if now.Sub(session.started) >= a.statusDelay() {
		a.postInitialStatus(ctx, session)
		return
	}
	if now.Sub(current.lastTyping) < a.typingInterval() {
		return
	}
	a.presenceMu.Lock()
	session.lastTyping = now
	a.presenceMu.Unlock()
	a.sendTyping(ctx, session.destination())
}

func (a *Adapter) postInitialStatus(ctx context.Context, session *presenceSession) {
	session.statusMu.Lock()
	defer session.statusMu.Unlock()
	current := a.snapshot(session)
	if current.statusID != 0 {
		return
	}
	display := current.lastText
	if display == "" {
		display = status.Fallback(current.updates)
	}
	_ = a.publishStatusLocked(ctx, session, display)
}

func (a *Adapter) PresenceUpdated(ctx context.Context, req *runtimechannelhost.DeliveryRequest, text, args string, failed bool) error {
	session, err := a.ensurePresence(req)
	if err != nil {
		return err
	}
	current := a.snapshot(session)
	if failed {
		activity := strings.TrimSpace(text)
		if activity == "" {
			activity = current.lastText
		}
		display := status.RenderFailure(activity, a.secretVault)
		a.presenceMu.Lock()
		if session.failedNotified {
			a.presenceMu.Unlock()
			return nil
		}
		session.failedNotified = true
		session.lastText = display
		a.presenceMu.Unlock()
		a.publishStatus(ctx, session, display)
		return nil
	}
	if strings.TrimSpace(text) == "" {
		text = current.lastText
	}
	display := status.Render(text, args, current.updates, a.secretVault)
	now := time.Now()
	a.presenceMu.Lock()
	session.lastText = display
	session.failedNotified = false
	a.presenceMu.Unlock()
	if current.statusID == 0 && now.Sub(session.started) < a.statusDelay() {
		if now.Sub(current.lastTyping) >= a.typingInterval() {
			a.presenceMu.Lock()
			session.lastTyping = now
			a.presenceMu.Unlock()
			a.sendTyping(ctx, session.destination())
		}
		return nil
	}
	a.coalescedStatusUpdate(ctx, session, display)
	return nil
}

func (a *Adapter) coalescedStatusUpdate(ctx context.Context, session *presenceSession, display string) {
	session.statusMu.Lock()
	defer session.statusMu.Unlock()
	if a.statusWait(session) > 0 {
		wakeClock(session)
		return
	}
	a.publishOrDefer(ctx, session, display)
}

func (a *Adapter) flushStatus(ctx context.Context, session *presenceSession) {
	session.statusMu.Lock()
	defer session.statusMu.Unlock()
	current := a.snapshot(session)
	if current.lastText == "" || current.lastText == current.shownText {
		return
	}
	if current.statusID == 0 && time.Since(session.started) < a.statusDelay() {
		return
	}
	if a.statusWait(session) > 0 {
		wakeClock(session)
		return
	}
	a.publishOrDefer(ctx, session, current.lastText)
}

func (a *Adapter) publishOrDefer(ctx context.Context, session *presenceSession, display string) {
	if err := a.publishStatusLocked(ctx, session, display); err != nil && a.statusWait(session) > 0 {
		wakeClock(session)
	}
}

func wakeClock(session *presenceSession) {
	select {
	case session.wake <- struct{}{}:
	default:
	}
}

func (a *Adapter) statusWait(session *presenceSession) time.Duration {
	current := a.snapshot(session)
	wait := time.Until(a.chatHeldUntil(session.chatID))
	if current.statusID != 0 {
		wait = max(wait, a.editInterval()-time.Since(current.lastEdit))
	}
	return max(wait, 0)
}

func (a *Adapter) publishStatus(ctx context.Context, session *presenceSession, display string) {
	session.statusMu.Lock()
	defer session.statusMu.Unlock()
	_ = a.publishStatusLocked(ctx, session, display)
}

func (a *Adapter) publishStatusLocked(ctx context.Context, session *presenceSession, display string) error {
	a.presenceMu.Lock()
	statusID := session.statusID
	shownText := session.shownText
	attempt := session.attempts
	if statusID == 0 || display != shownText {
		session.attempts++
	}
	a.presenceMu.Unlock()
	if statusID == 0 {
		id, err := a.postStatusMessage(ctx, session, attempt, display)
		if err != nil {
			a.logger.WarnContext(ctx, "status post failed", "component", "telegram", "error", err)
			return err
		}
		a.recordShown(session, id, display)
		return nil
	}
	if display == shownText {
		return nil
	}
	if err := a.editStatusMessage(ctx, session, statusID, attempt, display); err != nil {
		a.logger.WarnContext(ctx, "status edit failed", "component", "telegram", "error", err)
		return err
	}
	a.recordShown(session, statusID, display)
	return nil
}

func (a *Adapter) recordShown(session *presenceSession, statusID int64, display string) {
	a.presenceMu.Lock()
	defer a.presenceMu.Unlock()
	session.statusID = statusID
	session.shownText = display
	session.lastEdit = time.Now()
	session.updates++
}

func (a *Adapter) postStatusMessage(ctx context.Context, session *presenceSession, attempt int, text string) (int64, error) {
	request, err := json.Marshal(sendOperation{ChatID: session.chatID, ThreadID: session.threadID, Text: text, Status: true})
	if err != nil {
		return 0, Errorf(ErrorCodeProtocolInvalid, "status request is not serializable")
	}
	intent, err := a.effects.Execute(ctx, &effect.PrepareRequest{
		SessionID:      session.request.ConversationID,
		IdempotencyKey: statusKey(session.request.EffectID, session.chatID, "s"+strconv.Itoa(attempt), text),
		Provider:       ProviderName,
		Operation:      OpSendMessage,
		Classification: effect.ClassificationEffectful,
		Request:        request,
		EventKind:      effect.EventKindChannelRequested,
	}, a)
	if err != nil {
		return 0, err
	}
	return receiptMessageID(intent)
}

func (a *Adapter) editStatusMessage(ctx context.Context, session *presenceSession, messageID int64, attempt int, text string) error {
	request, err := json.Marshal(editOperation{ChatID: session.chatID, MessageID: messageID, Text: text})
	if err != nil {
		return Errorf(ErrorCodeProtocolInvalid, "status edit request is not serializable")
	}
	intent, err := a.effects.Execute(ctx, &effect.PrepareRequest{
		SessionID:      session.request.ConversationID,
		IdempotencyKey: statusKey(session.request.EffectID, session.chatID, "e"+strconv.Itoa(attempt), text),
		Provider:       ProviderName,
		Operation:      opEditMessage,
		Classification: effect.ClassificationEffectful,
		Request:        request,
		EventKind:      effect.EventKindChannelRequested,
	}, a)
	if err != nil {
		return err
	}
	_, err = receiptMessageID(intent)
	return err
}

func (s *presenceSession) destination() destination {
	return destination{chatID: s.chatID, threadID: s.threadID}
}

func (a *Adapter) sendTyping(ctx context.Context, target destination) {
	chatID := target.chatID
	if time.Until(a.chatHeldUntil(chatID)) > 0 {
		return
	}
	token, ok := a.botToken()
	if !ok {
		a.logger.WarnContext(ctx, "typing trigger failed", "component", "telegram", "error", faultBotRefUnresolved)
		return
	}
	call, cancel := context.WithTimeout(ctx, typingTimeout)
	defer cancel()
	err := a.bot.sendChatAction(call, token, target, chatActionTyping)
	if err == nil || ctx.Err() != nil {
		return
	}
	if apiErr, rejected := apiErrorOf(err); rejected && apiErr.Status == http.StatusTooManyRequests {
		a.holdChat(chatID, min(max(apiErr.RetryAfter, 0), a.rateCap))
	}
	a.logger.WarnContext(ctx, "typing trigger failed", "component", "telegram", "error", err)
}

func (a *Adapter) holdChat(chatID int64, delay time.Duration) {
	if delay <= 0 {
		return
	}
	now := time.Now()
	until := now.Add(delay)
	a.holdMu.Lock()
	defer a.holdMu.Unlock()
	for id, held := range a.holds {
		if !held.After(now) {
			delete(a.holds, id)
		}
	}
	if until.After(a.holds[chatID]) {
		a.holds[chatID] = until
	}
}

func (a *Adapter) chatHeldUntil(chatID int64) time.Time {
	a.holdMu.Lock()
	defer a.holdMu.Unlock()
	return a.holds[chatID]
}

func (a *Adapter) stopPresence(turnID string) {
	a.presenceMu.Lock()
	session := a.presence[turnID]
	var done chan struct{}
	if session != nil {
		if session.stop != nil && !session.stopped {
			close(session.stop)
		}
		session.stopped = true
		done = session.done
	}
	a.presenceMu.Unlock()
	if done != nil {
		<-done
	}
}

func (a *Adapter) PresenceEnded(_ context.Context, turnID string) error {
	if turnID == "" {
		return Errorf(ErrorCodeInvalidArgument, "presence turn id must not be empty")
	}
	a.stopPresence(turnID)
	a.presenceMu.Lock()
	delete(a.presence, turnID)
	a.presenceMu.Unlock()
	return nil
}

func (a *Adapter) statusDelay() time.Duration {
	if a.statusAfter > 0 {
		return a.statusAfter
	}
	return statusPostAfter
}

func (a *Adapter) typingInterval() time.Duration {
	if a.typingRefresh > 0 {
		return a.typingRefresh
	}
	return typingRefreshInterval
}

func (a *Adapter) editInterval() time.Duration {
	return time.Duration(a.cfg.MinEditInterval)
}
