package discord

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/anggasct/aura/internal/channels/kit/status"
	"github.com/anggasct/aura/internal/platform/effect"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/secret"
	runtimechannelhost "github.com/anggasct/aura/internal/platform/turn/channelhost"
)

const (
	typingRefreshInterval = 8 * time.Second
	statusPostAfter       = 10 * time.Second
	elapsedTickInterval   = 30 * time.Second
)

type presenceSession struct {
	channelID    string
	conversation string
	replyContext []byte
	channelKey   string
	request      *runtimechannelhost.DeliveryRequest
	started      time.Time
	lastTyping   time.Time
	statusID     string
	steps        []status.TrackedStep
	shownText    string
	lastEdit     time.Time
	lastTick     time.Time
	dirty        bool
	statusMu     sync.Mutex
	wake         chan struct{}
	stop         chan struct{}
	done         chan struct{}
	stopped      bool
}

type presenceSnapshot struct {
	channelID  string
	statusID   string
	shownText  string
	lastEdit   time.Time
	lastTick   time.Time
	lastTyping time.Time
	started    time.Time
	dirty      bool
	stopped    bool
}

func presenceKey(channel, effectID string) string {
	return channel + "|" + effectID
}

func (a *Adapter) vaultOrNil() *secret.Vault {
	if a == nil {
		return nil
	}
	return a.secretVault
}

func (a *Adapter) snapshot(session *presenceSession) presenceSnapshot {
	a.presenceMu.Lock()
	defer a.presenceMu.Unlock()
	return presenceSnapshot{
		channelID:  session.channelID,
		statusID:   session.statusID,
		shownText:  session.shownText,
		lastEdit:   session.lastEdit,
		lastTick:   session.lastTick,
		lastTyping: session.lastTyping,
		started:    session.started,
		dirty:      session.dirty,
		stopped:    session.stopped,
	}
}

func (a *Adapter) renderSession(session *presenceSession, now time.Time) string {
	a.presenceMu.Lock()
	steps := status.StepsOf(session.steps)
	started := session.started
	a.presenceMu.Unlock()
	return status.Render(steps, now.Sub(started), status.DefaultTexts(), a.vaultOrNil())
}

func (a *Adapter) triggerTyping(ctx context.Context, channelID string) error {
	if a == nil {
		return fault.New(ErrorCodeInvalidArgument, "adapter must not be nil")
	}
	if !isChannelID(channelID) {
		return fault.New(ErrorCodeInvalidArgument, "typing channel is not a discord channel id")
	}
	if time.Until(a.channelHeldUntil(channelID)) > 0 {
		return nil
	}
	token, err := a.tokenRef.Resolve()
	if err != nil {
		return fault.New(ErrorCodeConnectionFailed, "bot token is unavailable")
	}
	call, cancel := context.WithTimeout(ctx, restTimeout)
	defer cancel()
	endpoint := a.restBase + "/channels/" + channelID + "/typing"
	req, err := http.NewRequestWithContext(call, http.MethodPost, endpoint, bytes.NewReader([]byte{}))
	if err != nil {
		return fault.New(ErrorCodeProtocolInvalid, "typing request is not constructible")
	}
	req.Header.Set("Authorization", "Bot "+token)
	req.Header.Set("Content-Length", "0")
	resp, err := a.httpClient.Do(req)
	if err != nil {
		return fault.New(ErrorCodeConnectionFailed, "typing trigger did not reach discord")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		if delay := parseRetryAfter(resp.Header.Get("Retry-After")); delay > 0 {
			a.holdChannel(channelID, min(delay, a.rateCap))
		}
		return deliveryAmbiguous("typing trigger was rate limited")
	}
	if resp.StatusCode != http.StatusNoContent && (resp.StatusCode < 200 || resp.StatusCode >= 300) {
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return fault.New(ErrorCodeDeliveryFailed, "typing trigger was rejected")
		}
		if resp.StatusCode == http.StatusNotFound {
			return fault.New(ErrorCodeDeliveryFailed, "typing channel was not found")
		}
		return deliveryAmbiguous("typing trigger outcome is unknown")
	}
	return nil
}

func (a *Adapter) holdChannel(channelID string, delay time.Duration) {
	if delay <= 0 {
		return
	}
	now := time.Now()
	until := now.Add(delay)
	a.holdMu.Lock()
	defer a.holdMu.Unlock()
	if a.holds == nil {
		a.holds = map[string]time.Time{}
	}
	for id, held := range a.holds {
		if !held.After(now) {
			delete(a.holds, id)
		}
	}
	if until.After(a.holds[channelID]) {
		a.holds[channelID] = until
	}
}

func (a *Adapter) channelHeldUntil(channelID string) time.Time {
	a.holdMu.Lock()
	defer a.holdMu.Unlock()
	return a.holds[channelID]
}

func (a *Adapter) ensurePresence(req *runtimechannelhost.DeliveryRequest) (*presenceSession, string, error) {
	if req == nil {
		return nil, "", fault.New(ErrorCodeInvalidArgument, "presence request must not be nil")
	}
	if req.EffectID == "" {
		return nil, "", fault.New(ErrorCodeInvalidArgument, "presence request carries no turn id")
	}
	channelID := req.Channel
	if len(req.ReplyContext) > 0 {
		var reference replyReference
		if err := json.Unmarshal(req.ReplyContext, &reference); err != nil {
			return nil, "", fault.New(ErrorCodeProtocolInvalid, "reply context is not decodable")
		}
		channelID, _, _ = deliveryTarget(&reference)
	}
	a.presenceMu.Lock()
	defer a.presenceMu.Unlock()
	if a.presence == nil {
		a.presence = map[string]*presenceSession{}
	}
	session, ok := a.presence[req.EffectID]
	if !ok {
		now := time.Now()
		cloned := *req
		cloned.Parts = nil
		if len(req.ReplyContext) > 0 {
			cloned.ReplyContext = append([]byte(nil), req.ReplyContext...)
		}
		session = &presenceSession{
			channelID:    channelID,
			conversation: req.ConversationID,
			channelKey:   presenceKey(req.Channel, req.EffectID),
			request:      &cloned,
			started:      now,
			wake:         make(chan struct{}, 1),
		}
		if len(req.ReplyContext) > 0 {
			session.replyContext = append([]byte(nil), req.ReplyContext...)
		}
		a.presence[req.EffectID] = session
	}
	return session, channelID, nil
}

func (a *Adapter) PresenceStarted(ctx context.Context, req *runtimechannelhost.DeliveryRequest) error {
	if req == nil {
		return fault.New(ErrorCodeInvalidArgument, "presence request must not be nil")
	}
	session, channelID, err := a.ensurePresence(req)
	if err != nil {
		return err
	}
	now := time.Now()
	a.presenceMu.Lock()
	session.lastTyping = now
	startClock := false
	if session.stop == nil && !session.stopped {
		session.stop = make(chan struct{})
		session.done = make(chan struct{})
		startClock = true
	}
	a.presenceMu.Unlock()
	if startClock {
		go a.runPresenceClock(ctx, session)
	}
	if err := a.triggerTyping(context.WithoutCancel(ctx), channelID); err != nil {
		a.logger.WarnContext(ctx, "typing trigger failed", "component", "discord")
	}
	return nil
}

func (a *Adapter) runPresenceClock(ctx context.Context, session *presenceSession) {
	a.presenceMu.Lock()
	stop := session.stop
	done := session.done
	delay := max(a.statusDelay()-time.Since(session.started), 0)
	interval := a.typingInterval()
	a.presenceMu.Unlock()
	defer func() {
		if done != nil {
			close(done)
		}
	}()

	statusTimer := time.NewTimer(delay)
	defer statusTimer.Stop()
	typingTicker := time.NewTicker(interval)
	defer typingTicker.Stop()
	flushTimer := time.NewTimer(interval)
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
	if err := a.triggerTyping(context.WithoutCancel(ctx), current.channelID); err != nil {
		a.logger.WarnContext(ctx, "typing refresh failed", "component", "discord")
	}
}

func (a *Adapter) postInitialStatus(ctx context.Context, session *presenceSession) {
	session.statusMu.Lock()
	defer session.statusMu.Unlock()
	current := a.snapshot(session)
	if current.statusID != "" || current.stopped {
		return
	}
	display := a.renderSession(session, time.Now())
	a.publishOrDefer(ctx, session, display)
}

func (a *Adapter) PresenceUpdated(ctx context.Context, req *runtimechannelhost.DeliveryRequest, update runtimechannelhost.PresenceUpdate) error {
	if req == nil {
		return fault.New(ErrorCodeInvalidArgument, "presence request must not be nil")
	}
	session, channelID, err := a.ensurePresence(req)
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
	session.dirty = true
	statusID := session.statusID
	started := session.started
	lastTyping := session.lastTyping
	a.presenceMu.Unlock()

	if statusID == "" && now.Sub(started) < a.statusDelay() {
		if now.Sub(lastTyping) >= a.typingInterval() {
			a.presenceMu.Lock()
			session.lastTyping = now
			a.presenceMu.Unlock()
			if err := a.triggerTyping(context.WithoutCancel(ctx), channelID); err != nil {
				a.logger.WarnContext(ctx, "typing refresh failed", "component", "discord")
			}
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
	display := a.renderSession(session, time.Now())
	a.publishOrDefer(ctx, session, display)
}

func (a *Adapter) flushStatus(ctx context.Context, session *presenceSession) {
	session.statusMu.Lock()
	defer session.statusMu.Unlock()
	current := a.snapshot(session)
	if !current.dirty || current.stopped {
		return
	}
	if current.statusID == "" && time.Since(current.started) < a.statusDelay() {
		return
	}
	if a.statusWait(session) > 0 {
		wakeClock(session)
		return
	}
	display := a.renderSession(session, time.Now())
	if current.statusID != "" && display == current.shownText {
		a.clearDirty(session)
		return
	}
	a.publishOrDefer(ctx, session, display)
}

func (a *Adapter) tickElapsed(ctx context.Context, session *presenceSession) {
	session.statusMu.Lock()
	defer session.statusMu.Unlock()
	current := a.snapshot(session)
	if current.statusID == "" || current.stopped {
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
	display := a.renderSession(session, now)
	if display == current.shownText {
		return
	}
	a.publishOrDefer(ctx, session, display)
}

func (a *Adapter) publishOrDefer(ctx context.Context, session *presenceSession, display string) {
	if err := a.postOrPatchStatusLocked(ctx, session, display); err != nil && a.statusWait(session) > 0 {
		a.markDirty(session)
		wakeClock(session)
	}
}

func (a *Adapter) markDirty(session *presenceSession) {
	a.presenceMu.Lock()
	session.dirty = true
	a.presenceMu.Unlock()
}

func (a *Adapter) clearDirty(session *presenceSession) {
	a.presenceMu.Lock()
	session.dirty = false
	a.presenceMu.Unlock()
}

func wakeClock(session *presenceSession) {
	if session.wake == nil {
		return
	}
	select {
	case session.wake <- struct{}{}:
	default:
	}
}

func (a *Adapter) statusWait(session *presenceSession) time.Duration {
	current := a.snapshot(session)
	wait := time.Until(a.channelHeldUntil(current.channelID))
	if current.statusID != "" {
		wait = max(wait, time.Duration(a.cfg.MinEditInterval)-time.Since(current.lastEdit))
	}
	return max(wait, 0)
}

func (a *Adapter) idleWait(session *presenceSession) (time.Duration, bool) {
	current := a.snapshot(session)
	if current.statusID == "" || current.stopped {
		return 0, false
	}
	base := current.lastTick
	if base.IsZero() {
		base = current.lastEdit
	}
	wait := max(a.elapsedInterval()-time.Since(base), 0)
	return max(wait, a.statusWait(session)), true
}

func (a *Adapter) postOrPatchStatusLocked(ctx context.Context, session *presenceSession, display string) error {
	a.presenceMu.Lock()
	statusID := session.statusID
	shownText := session.shownText
	request := session.request
	channelID := session.channelID
	a.presenceMu.Unlock()
	if request == nil {
		return fault.New(ErrorCodeInvalidArgument, "presence request must not be nil")
	}
	if statusID == "" {
		id, postErr := a.postPresenceMessage(ctx, request, channelID, display)
		if postErr != nil || id == "" {
			a.logger.WarnContext(ctx, "status post failed", "component", "discord")
			return postErr
		}
		now := time.Now()
		a.presenceMu.Lock()
		session.statusID = id
		session.shownText = display
		session.lastEdit = now
		session.lastTick = now
		session.dirty = false
		a.presenceMu.Unlock()
		a.rememberPosted(session.channelKey, []string{id})
		wakeClock(session)
		return nil
	}
	if display == shownText {
		a.clearDirty(session)
		return nil
	}
	if patchErr := a.patchPresenceMessage(ctx, request, channelID, statusID, display); patchErr != nil {
		a.logger.WarnContext(ctx, "status edit failed", "component", "discord")
		return patchErr
	}
	now := time.Now()
	a.presenceMu.Lock()
	session.shownText = display
	session.lastEdit = now
	session.lastTick = now
	session.dirty = false
	a.presenceMu.Unlock()
	wakeClock(session)
	return nil
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

func (a *Adapter) PresenceEnded(ctx context.Context, turnID string) error {
	if turnID == "" {
		return fault.New(ErrorCodeInvalidArgument, "presence turn id must not be empty")
	}
	a.stopPresence(turnID)
	a.presenceMu.Lock()
	delete(a.presence, turnID)
	a.presenceMu.Unlock()
	return nil
}

func (a *Adapter) statusDelay() time.Duration {
	if a != nil && a.statusAfter > 0 {
		return a.statusAfter
	}
	return statusPostAfter
}

func (a *Adapter) typingInterval() time.Duration {
	if a != nil && a.typingRefresh > 0 {
		return a.typingRefresh
	}
	return typingRefreshInterval
}

func (a *Adapter) elapsedInterval() time.Duration {
	if a != nil && a.elapsedTick > 0 {
		return a.elapsedTick
	}
	return elapsedTickInterval
}

func (a *Adapter) postPresenceMessage(ctx context.Context, req *runtimechannelhost.DeliveryRequest, channelID, text string) (string, error) {
	operation := SendOperation{ChannelID: channelID, Text: text}
	request, err := marshalOperation(operation)
	if err != nil {
		return "", err
	}
	intent, err := a.effects.Execute(ctx, &effect.PrepareRequest{
		SessionID:      req.ConversationID,
		IdempotencyKey: idempotencyKey(req.EffectID, channelID, "s0", text),
		Provider:       "discord",
		Operation:      OpSendMessage,
		Classification: effect.ClassificationEffectful,
		Request:        request,
		EventKind:      effect.EventKindChannelRequested,
	}, a)
	if err != nil {
		return "", err
	}
	return receiptMessageID(intent)
}

func (a *Adapter) patchPresenceMessage(ctx context.Context, req *runtimechannelhost.DeliveryRequest, channelID, messageID, text string) error {
	request, err := marshalOperation(editOperation{ChannelID: channelID, MessageID: messageID, Text: text})
	if err != nil {
		return err
	}
	intent, err := a.effects.Execute(ctx, &effect.PrepareRequest{
		SessionID:      req.ConversationID,
		IdempotencyKey: idempotencyKey(req.EffectID, channelID, "se0", text),
		Provider:       "discord",
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
