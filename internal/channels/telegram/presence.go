package telegram

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/anggasct/aura/internal/channels/kit/approval"
	"github.com/anggasct/aura/internal/channels/kit/status"
	"github.com/anggasct/aura/internal/platform/effect"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/principal"
	runtimechannelhost "github.com/anggasct/aura/internal/platform/turn/channelhost"
)

const (
	typingRefreshInterval = 4 * time.Second
	statusPostAfter       = 10 * time.Second
	elapsedTickInterval   = 30 * time.Second
	typingTimeout         = 10 * time.Second
	chatActionTyping      = "typing"
	maxPresenceSessions   = 256
)

var _ runtimechannelhost.PresencePort = (*Adapter)(nil)

type presenceSession struct {
	chatID       int64
	threadID     int64
	fromID       int64
	request      *runtimechannelhost.DeliveryRequest
	started      time.Time
	lastTyping   time.Time
	statusID     int64
	stopTokenID  string
	stopPending  *approval.Pending
	stopCallback string
	steps        []status.TrackedStep
	shownText    string
	lastEdit     time.Time
	lastTick     time.Time
	attempts     int
	version      uint64
	dirty        bool
	statusMu     sync.Mutex
	wake         chan struct{}
	stop         chan struct{}
	done         chan struct{}
	stopped      bool
}

type presenceSnapshot struct {
	statusID   int64
	shownText  string
	lastEdit   time.Time
	lastTick   time.Time
	lastTyping time.Time
	dirty      bool
	stopped    bool
}

func (a *Adapter) snapshot(session *presenceSession) presenceSnapshot {
	a.presenceMu.Lock()
	defer a.presenceMu.Unlock()
	return presenceSnapshot{
		statusID:   session.statusID,
		shownText:  session.shownText,
		lastEdit:   session.lastEdit,
		lastTick:   session.lastTick,
		lastTyping: session.lastTyping,
		dirty:      session.dirty,
		stopped:    session.stopped,
	}
}

func (a *Adapter) renderSession(session *presenceSession, now time.Time) (display string, version uint64) {
	a.presenceMu.Lock()
	steps := status.StepsOf(session.steps)
	started := session.started
	version = session.version
	a.presenceMu.Unlock()
	return status.Render(steps, now.Sub(started), status.DefaultTexts(), a.secretVault), version
}

func (a *Adapter) ensurePresence(req *runtimechannelhost.DeliveryRequest) (*presenceSession, error) {
	if req == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "presence request must not be nil")
	}
	if req.EffectID == "" {
		return nil, fault.New(ErrorCodeInvalidArgument, "presence request carries no turn id")
	}
	var target replyReference
	if err := json.Unmarshal(req.ReplyContext, &target); err != nil || target.ChatID == 0 {
		return nil, fault.New(ErrorCodeInvalidArgument, "presence reply context carries no chat")
	}
	a.presenceMu.Lock()
	defer a.presenceMu.Unlock()
	if session, ok := a.presence[req.EffectID]; ok {
		return session, nil
	}
	if len(a.presence) >= maxPresenceSessions {
		return nil, fault.New(ErrorCodeDeliveryUnavailable, "presence session capacity is exhausted")
	}
	cloned := *req
	cloned.Parts = nil
	cloned.ReplyContext = slices.Clone(req.ReplyContext)
	session := &presenceSession{
		chatID:   target.ChatID,
		threadID: target.ThreadID,
		fromID:   target.FromID,
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
	idleTimer := time.NewTimer(a.elapsedInterval())
	idleTimer.Stop()
	defer idleTimer.Stop()

	armIdle := func() {
		if wait, ok := a.idleWait(session); ok {
			idleTimer.Reset(wait)
		}
	}

	for {
		select {
		case <-stop:
			return
		case <-ctx.Done():
			return
		default:
		}
		select {
		case <-stop:
			return
		case <-ctx.Done():
			return
		case <-typingTicker.C:
			a.refreshTyping(ctx, session)
		case <-statusTimer.C:
			a.postInitialStatus(ctx, session)
			armIdle()
		case <-session.wake:
			if a.snapshot(session).dirty {
				flushTimer.Reset(a.statusWait(session))
			}
			armIdle()
		case <-flushTimer.C:
			a.flushStatus(ctx, session)
			armIdle()
		case <-idleTimer.C:
			a.tickElapsed(ctx, session)
			armIdle()
		}
	}
}

func (a *Adapter) refreshTyping(ctx context.Context, session *presenceSession) {
	current := a.snapshot(session)
	if current.stopped {
		return
	}
	now := time.Now()
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
	if current.statusID != 0 || current.stopped {
		return
	}
	if a.statusWait(session) > 0 {
		a.markDirty(session)
		wakeClock(session)
		return
	}
	display, version := a.renderSession(session, time.Now())
	a.publishOrDefer(ctx, session, display, version)
}

func (a *Adapter) PresenceUpdated(ctx context.Context, req *runtimechannelhost.DeliveryRequest, update runtimechannelhost.PresenceUpdate) error {
	session, err := a.ensurePresence(req)
	if err != nil {
		return err
	}
	now := time.Now()
	a.presenceMu.Lock()
	if session.stopped {
		a.presenceMu.Unlock()
		return nil
	}
	session.steps = status.ApplyUpdate(session.steps, update)
	session.version++
	session.dirty = true
	statusID := session.statusID
	started := session.started
	lastTyping := session.lastTyping
	a.presenceMu.Unlock()
	if statusID == 0 && now.Sub(started) < a.statusDelay() {
		if now.Sub(lastTyping) >= a.typingInterval() {
			a.presenceMu.Lock()
			session.lastTyping = now
			a.presenceMu.Unlock()
			a.sendTyping(ctx, session.destination())
		}
		return nil
	}
	a.coalescedStatusUpdate(ctx, session)
	return nil
}

func (a *Adapter) coalescedStatusUpdate(ctx context.Context, session *presenceSession) {
	session.statusMu.Lock()
	defer session.statusMu.Unlock()
	if a.snapshot(session).stopped {
		return
	}
	if a.statusWait(session) > 0 {
		wakeClock(session)
		return
	}
	display, version := a.renderSession(session, time.Now())
	a.publishOrDefer(ctx, session, display, version)
}

func (a *Adapter) flushStatus(ctx context.Context, session *presenceSession) {
	session.statusMu.Lock()
	defer session.statusMu.Unlock()
	current := a.snapshot(session)
	if !current.dirty || current.stopped {
		return
	}
	if current.statusID == 0 && time.Since(session.started) < a.statusDelay() {
		return
	}
	if a.statusWait(session) > 0 {
		wakeClock(session)
		return
	}
	display, version := a.renderSession(session, time.Now())
	if current.statusID != 0 && display == current.shownText {
		a.clearDirtyIfCurrent(session, version)
		return
	}
	a.publishOrDefer(ctx, session, display, version)
}

func (a *Adapter) tickElapsed(ctx context.Context, session *presenceSession) {
	session.statusMu.Lock()
	defer session.statusMu.Unlock()
	current := a.snapshot(session)
	if current.statusID == 0 || current.stopped {
		return
	}
	now := time.Now()
	base := current.lastTick
	if base.IsZero() {
		base = current.lastEdit
	}
	if now.Sub(base) < a.elapsedInterval() {
		return
	}
	if a.statusWait(session) > 0 {
		a.markDirty(session)
		wakeClock(session)
		return
	}
	a.presenceMu.Lock()
	session.lastTick = now
	a.presenceMu.Unlock()
	display, version := a.renderSession(session, now)
	if display == current.shownText {
		return
	}
	a.publishOrDefer(ctx, session, display, version)
}

func (a *Adapter) publishOrDefer(ctx context.Context, session *presenceSession, display string, version uint64) {
	if err := a.publishStatusLocked(ctx, session, display, version); err != nil && a.statusWait(session) > 0 {
		a.markDirty(session)
		wakeClock(session)
	}
}

func (a *Adapter) markDirty(session *presenceSession) {
	a.presenceMu.Lock()
	session.version++
	session.dirty = true
	a.presenceMu.Unlock()
}

func (a *Adapter) clearDirtyIfCurrent(session *presenceSession, renderedVersion uint64) {
	a.presenceMu.Lock()
	session.dirty = session.version != renderedVersion
	a.presenceMu.Unlock()
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

func (a *Adapter) idleWait(session *presenceSession) (time.Duration, bool) {
	current := a.snapshot(session)
	if current.statusID == 0 || current.stopped {
		return 0, false
	}
	base := current.lastTick
	if base.IsZero() {
		base = current.lastEdit
	}
	wait := max(a.elapsedInterval()-time.Since(base), 0)
	return max(wait, a.statusWait(session)), true
}

func (a *Adapter) publishStatusLocked(ctx context.Context, session *presenceSession, display string, renderedVersion uint64) error {
	a.presenceMu.Lock()
	statusID := session.statusID
	shownText := session.shownText
	stopCallback := session.stopCallback
	attempt := session.attempts
	if statusID == 0 || display != shownText {
		session.attempts++
	}
	a.presenceMu.Unlock()
	if statusID == 0 {
		tokenID, pending, callbackData, minted := a.mintStopToken(session)
		id, err := a.postStatusMessage(ctx, session, attempt, display, stopKeyboard(callbackData))
		if err != nil {
			if minted {
				a.stops.Drop(tokenID, pending)
			}
			a.logger.WarnContext(ctx, "status post failed", "component", "telegram", "error", err)
			return err
		}
		if minted {
			a.presenceMu.Lock()
			session.stopTokenID = tokenID
			session.stopPending = pending
			session.stopCallback = callbackData
			a.presenceMu.Unlock()
		}
		a.recordShown(session, id, display, renderedVersion)
		return nil
	}
	if display == shownText {
		a.clearDirtyIfCurrent(session, renderedVersion)
		return nil
	}
	if err := a.editStatusMessage(ctx, session, statusID, attempt, display, stopKeyboard(stopCallback)); err != nil {
		a.logger.WarnContext(ctx, "status edit failed", "component", "telegram", "error", err)
		return err
	}
	a.recordShown(session, statusID, display, renderedVersion)
	return nil
}

func (a *Adapter) mintStopToken(session *presenceSession) (tokenID string, pending *approval.Pending, callbackData string, ok bool) {
	if a.stops == nil || session == nil || session.request == nil {
		return "", nil, "", false
	}
	if principal.ParseClass(session.request.PrincipalClass) != principal.ClassOwner {
		return "", nil, "", false
	}
	if session.fromID <= 0 || session.request.Deadline.IsZero() {
		return "", nil, "", false
	}
	binding := &approval.Binding{
		Class:     principal.ClassOwner,
		Actor:     strconv.FormatInt(session.fromID, 10),
		Scope:     scopeOf(session.destination()),
		SessionID: session.request.ConversationID,
		TurnID:    session.request.EffectID,
	}
	tokenID, pending, err := a.stops.Issue(binding, session.request.Deadline)
	if err != nil {
		return "", nil, "", false
	}
	return tokenID, pending, a.stops.Callback(tokenID, approval.ActionStop, binding, session.request.Deadline), true
}

func stopKeyboard(callbackData string) *inlineKeyboard {
	if callbackData == "" {
		return nil
	}
	return &inlineKeyboard{Rows: [][]inlineButton{{{
		Text:         status.DefaultTexts().StopButton,
		CallbackData: callbackData,
	}}}}
}

func (a *Adapter) recordShown(session *presenceSession, statusID int64, display string, renderedVersion uint64) {
	now := time.Now()
	a.presenceMu.Lock()
	session.statusID = statusID
	session.shownText = display
	session.lastEdit = now
	session.lastTick = now
	session.dirty = session.version != renderedVersion
	a.presenceMu.Unlock()
	wakeClock(session)
}

func (a *Adapter) postStatusMessage(ctx context.Context, session *presenceSession, attempt int, text string, markup *inlineKeyboard) (int64, error) {
	request, err := json.Marshal(sendOperation{ChatID: session.chatID, ThreadID: session.threadID, Text: text, ReplyMarkup: markup, Status: true})
	if err != nil {
		return 0, fault.New(ErrorCodeProtocolInvalid, "status request is not serializable")
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

func (a *Adapter) editStatusMessage(ctx context.Context, session *presenceSession, messageID int64, attempt int, text string, markup *inlineKeyboard) error {
	request, err := json.Marshal(editOperation{ChatID: session.chatID, MessageID: messageID, Text: text, ReplyMarkup: markup})
	if err != nil {
		return fault.New(ErrorCodeProtocolInvalid, "status edit request is not serializable")
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

func (a *Adapter) clearPresenceStop(ctx context.Context, turnID string) {
	a.presenceMu.Lock()
	session := a.presence[turnID]
	a.presenceMu.Unlock()
	if session == nil {
		return
	}
	session.statusMu.Lock()
	defer session.statusMu.Unlock()
	a.presenceMu.Lock()
	statusID := session.statusID
	chatID := session.chatID
	tokenID := session.stopTokenID
	pending := session.stopPending
	hadButton := session.stopCallback != ""
	session.stopTokenID = ""
	session.stopPending = nil
	session.stopCallback = ""
	a.presenceMu.Unlock()
	if tokenID != "" && a.stops != nil {
		a.stops.Drop(tokenID, pending)
	}
	if statusID != 0 && hadButton {
		a.clearStatusKeyboard(ctx, chatID, statusID)
	}
}

func (a *Adapter) takeStatusToDelete(turnID string) (chatID, statusID int64, ok bool) {
	a.presenceMu.Lock()
	session := a.presence[turnID]
	a.presenceMu.Unlock()
	if session == nil {
		return 0, 0, false
	}
	session.statusMu.Lock()
	defer session.statusMu.Unlock()
	a.presenceMu.Lock()
	defer a.presenceMu.Unlock()
	if session.statusID == 0 {
		return 0, 0, false
	}
	chatID = session.chatID
	statusID = session.statusID
	session.statusID = 0
	return chatID, statusID, true
}

func (a *Adapter) clearStatusKeyboard(ctx context.Context, chatID, messageID int64) {
	token, ok := a.botToken()
	if !ok {
		return
	}
	call, cancel := context.WithTimeout(context.WithoutCancel(ctx), noticeTimeout)
	defer cancel()
	if err := a.bot.clearKeyboard(call, token, chatID, messageID); err != nil {
		a.logger.WarnContext(ctx, "status keyboard removal failed", "component", "telegram", "account", a.account.Name)
	}
}

func (a *Adapter) PresenceEnded(ctx context.Context, turnID string, end runtimechannelhost.PresenceEnd) error {
	if turnID == "" {
		return fault.New(ErrorCodeInvalidArgument, "presence turn id must not be empty")
	}
	a.stopPresence(turnID)
	a.presenceMu.Lock()
	session := a.presence[turnID]
	delete(a.presence, turnID)
	a.presenceMu.Unlock()
	if session == nil {
		return nil
	}
	session.statusMu.Lock()
	defer session.statusMu.Unlock()
	a.presenceMu.Lock()
	statusID := session.statusID
	chatID := session.chatID
	tokenID := session.stopTokenID
	pending := session.stopPending
	hadButton := session.stopCallback != ""
	attempt := session.attempts
	if end.Stopped && statusID != 0 {
		session.attempts++
	}
	session.stopTokenID = ""
	session.stopPending = nil
	session.stopCallback = ""
	a.presenceMu.Unlock()
	if tokenID != "" && a.stops != nil {
		a.stops.Drop(tokenID, pending)
	}
	if statusID == 0 {
		return nil
	}
	if !end.Stopped {
		if hadButton {
			a.clearStatusKeyboard(ctx, chatID, statusID)
		}
		return nil
	}
	if time.Until(a.chatHeldUntil(chatID)) > 0 {
		return nil
	}
	if err := a.editStatusMessage(context.WithoutCancel(ctx), session, statusID, attempt, status.DefaultTexts().Stopped, nil); err != nil {
		a.logger.WarnContext(ctx, "status stopped edit failed", "component", "telegram", "error", err)
		if hadButton {
			a.clearStatusKeyboard(ctx, chatID, statusID)
		}
	}
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

func (a *Adapter) elapsedInterval() time.Duration {
	if a.elapsedTick > 0 {
		return a.elapsedTick
	}
	return elapsedTickInterval
}

func (a *Adapter) editInterval() time.Duration {
	return time.Duration(a.cfg.MinEditInterval)
}
