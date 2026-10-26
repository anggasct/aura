package discord

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/anggasct/aura/internal/effect"
	runtimechannelhost "github.com/anggasct/aura/internal/runtime/channelhost"
	"github.com/anggasct/aura/internal/secret"
)

const (
	typingRefreshInterval = 8 * time.Second
	statusPostAfter       = 10 * time.Second
	maxStatusWords        = 6
	maxArgRunes           = 80
	maxStatusRunes        = 280
)

var fallbackStatuses = [3]string{"Working…", "Still working…", "Wrapping up…"}

type presenceSession struct {
	channelID      string
	conversation   string
	replyContext   []byte
	channelKey     string
	request        *runtimechannelhost.DeliveryRequest
	started        time.Time
	lastTyping     time.Time
	statusID       string
	lastText       string
	shownText      string
	lastEdit       time.Time
	updates        int
	failedNotified bool
	statusMu       sync.Mutex
	stop           chan struct{}
	done           chan struct{}
	stopped        bool
}

func (a *Adapter) SetSecretVault(vault *secret.Vault) {
	if a == nil {
		return
	}
	a.secretVault = vault
}

func presenceKey(channel, effectID string) string {
	return channel + "|" + effectID
}

func truncateWords(s string, n int) string {
	words := strings.Fields(s)
	if len(words) <= n {
		return strings.Join(words, " ")
	}
	return strings.Join(words[:n], " ")
}

func truncateRunes(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n])
}

func fallbackStatus(count int) string {
	if count < 0 {
		count = 0
	}
	return fallbackStatuses[count%len(fallbackStatuses)]
}

func failureLine(activity string) string {
	activity = strings.TrimSpace(activity)
	activity = strings.TrimSuffix(activity, ".")
	if activity == "" {
		return "Couldn't finish that step, continuing."
	}
	return "Couldn't finish " + activity + ", continuing."
}

func containsSecretKeyword(s string) bool {
	lowered := strings.ToLower(s)
	for _, keyword := range []string{"password", "secret", "token", "api_key", "apikey", "bearer", "authorization", "private_key", "client_secret"} {
		if strings.Contains(lowered, keyword) {
			return true
		}
	}
	return false
}

func humanizeArgs(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	var decoded any
	if err := json.Unmarshal([]byte(trimmed), &decoded); err != nil {
		return truncateRunes(trimmed, maxArgRunes)
	}
	var parts []string
	var collect func(v any)
	collect = func(v any) {
		switch value := v.(type) {
		case string:
			if strings.TrimSpace(value) != "" {
				parts = append(parts, value)
			}
		case map[string]any:
			for _, item := range value {
				collect(item)
			}
		case []any:
			for _, item := range value {
				collect(item)
			}
		}
	}
	collect(decoded)
	if len(parts) == 0 {
		return ""
	}
	return truncateRunes(strings.Join(parts, " "), maxArgRunes)
}

func (a *Adapter) vaultOrNil() *secret.Vault {
	if a == nil {
		return nil
	}
	return a.secretVault
}

func renderStatusText(raw, args string, updateCount int, vault *secret.Vault) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return fallbackStatus(updateCount)
	}
	short := truncateWords(trimmed, maxStatusWords)
	short = truncateRunes(strings.TrimSpace(short), maxStatusRunes)
	if short == "" {
		return fallbackStatus(updateCount)
	}
	if vault != nil {
		short = vault.Redact(short)
	}
	if containsSecretKeyword(short) {
		return fallbackStatus(updateCount)
	}
	argText := humanizeArgs(args)
	secretHit := false
	switch {
	case vault != nil && argText != "" && vault.Contains(argText):
		secretHit = true
	case vault != nil && args != "" && vault.Contains(args):
		secretHit = true
	case containsSecretKeyword(args) || containsSecretKeyword(argText):
		secretHit = true
	}
	if secretHit || argText == "" {
		return short
	}
	if vault != nil {
		argText = vault.Redact(argText)
	}
	return truncateRunes(short+" "+argText, maxStatusRunes)
}

func (a *Adapter) triggerTyping(ctx context.Context, channelID string) error {
	if a == nil {
		return Errorf(ErrorCodeInvalidArgument, "adapter must not be nil")
	}
	if !isChannelID(channelID) {
		return Errorf(ErrorCodeInvalidArgument, "typing channel is not a discord channel id")
	}
	token, err := a.tokenRef.Resolve()
	if err != nil {
		return Errorf(ErrorCodeConnectionFailed, "bot token is unavailable")
	}
	call, cancel := context.WithTimeout(ctx, restTimeout)
	defer cancel()
	endpoint := a.restBase + "/channels/" + channelID + "/typing"
	req, err := http.NewRequestWithContext(call, http.MethodPost, endpoint, bytes.NewReader([]byte{}))
	if err != nil {
		return Errorf(ErrorCodeProtocolInvalid, "typing request is not constructible")
	}
	req.Header.Set("Authorization", "Bot "+token)
	req.Header.Set("Content-Length", "0")
	resp, err := a.httpClient.Do(req)
	if err != nil {
		return Errorf(ErrorCodeConnectionFailed, "typing trigger did not reach discord")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		return Errorf(ErrorCodeDeliveryAmbiguous, "typing trigger was rate limited")
	}
	if resp.StatusCode != http.StatusNoContent && (resp.StatusCode < 200 || resp.StatusCode >= 300) {
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return Errorf(ErrorCodeDeliveryFailed, "typing trigger was rejected")
		}
		if resp.StatusCode == http.StatusNotFound {
			return Errorf(ErrorCodeDeliveryFailed, "typing channel was not found")
		}
		return Errorf(ErrorCodeDeliveryAmbiguous, "typing trigger outcome is unknown")
	}
	return nil
}

func (a *Adapter) ensurePresence(req *runtimechannelhost.DeliveryRequest) (*presenceSession, string, error) {
	if req == nil {
		return nil, "", Errorf(ErrorCodeInvalidArgument, "presence request must not be nil")
	}
	if req.EffectID == "" {
		return nil, "", Errorf(ErrorCodeInvalidArgument, "presence request carries no turn id")
	}
	channelID := req.Channel
	if len(req.ReplyContext) > 0 {
		var reference replyReference
		if err := json.Unmarshal(req.ReplyContext, &reference); err != nil {
			return nil, "", Errorf(ErrorCodeProtocolInvalid, "reply context is not decodable")
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
			lastTyping:   time.Time{},
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
		return Errorf(ErrorCodeInvalidArgument, "presence request must not be nil")
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
	delay := a.statusDelay() - time.Since(session.started)
	if delay < 0 {
		delay = 0
	}
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
		}
	}
}

func (a *Adapter) refreshPresence(ctx context.Context, session *presenceSession) {
	a.presenceMu.Lock()
	statusID := session.statusID
	started := session.started
	lastTyping := session.lastTyping
	channelID := session.channelID
	a.presenceMu.Unlock()
	if statusID != "" {
		return
	}
	now := time.Now()
	if now.Sub(started) >= a.statusDelay() {
		a.postInitialStatus(ctx, session)
		return
	}
	if now.Sub(lastTyping) < a.typingInterval() {
		return
	}
	a.presenceMu.Lock()
	session.lastTyping = now
	a.presenceMu.Unlock()
	if err := a.triggerTyping(context.WithoutCancel(ctx), channelID); err != nil {
		a.logger.WarnContext(ctx, "typing refresh failed", "component", "discord")
	}
}

func (a *Adapter) postInitialStatus(ctx context.Context, session *presenceSession) {
	a.presenceMu.Lock()
	statusID := session.statusID
	display := session.lastText
	updates := session.updates
	a.presenceMu.Unlock()
	if statusID != "" {
		return
	}
	if display == "" {
		display = fallbackStatus(updates)
	}
	_ = a.postOrPatchStatus(ctx, session, display)
}

func (a *Adapter) PresenceUpdated(ctx context.Context, req *runtimechannelhost.DeliveryRequest, text, args string, failed bool) error {
	if req == nil {
		return Errorf(ErrorCodeInvalidArgument, "presence request must not be nil")
	}
	session, channelID, err := a.ensurePresence(req)
	if err != nil {
		return err
	}
	a.presenceMu.Lock()
	updates := session.updates
	lastText := session.lastText
	statusID := session.statusID
	started := session.started
	lastTyping := session.lastTyping
	failedNotified := session.failedNotified
	a.presenceMu.Unlock()

	if failed {
		if failedNotified {
			return nil
		}
		activity := strings.TrimSpace(text)
		if activity == "" {
			activity = lastText
		}
		display := failureLine(truncateWords(activity, maxStatusWords))
		a.presenceMu.Lock()
		session.failedNotified = true
		session.lastText = display
		a.presenceMu.Unlock()
		_ = a.postOrPatchStatus(ctx, session, display)
		return nil
	}
	if strings.TrimSpace(text) == "" {
		text = lastText
	}
	display := renderStatusText(text, args, updates, a.vaultOrNil())
	if display == "" {
		return nil
	}
	now := time.Now()
	a.presenceMu.Lock()
	session.lastText = display
	session.failedNotified = false
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
	_ = a.throttledStatusUpdate(ctx, session, display)
	return nil
}

func (a *Adapter) throttledStatusUpdate(ctx context.Context, session *presenceSession, display string) error {
	session.statusMu.Lock()
	defer session.statusMu.Unlock()
	a.presenceMu.Lock()
	statusID := session.statusID
	lastEdit := session.lastEdit
	a.presenceMu.Unlock()
	if statusID != "" && time.Since(lastEdit) < time.Duration(a.cfg.MinEditInterval) {
		return nil
	}
	return a.postOrPatchStatusLocked(ctx, session, display)
}

func (a *Adapter) postOrPatchStatus(ctx context.Context, session *presenceSession, display string) error {
	session.statusMu.Lock()
	defer session.statusMu.Unlock()
	return a.postOrPatchStatusLocked(ctx, session, display)
}

func (a *Adapter) postOrPatchStatusLocked(ctx context.Context, session *presenceSession, display string) error {
	a.presenceMu.Lock()
	statusID := session.statusID
	shownText := session.shownText
	updates := session.updates
	request := session.request
	channelID := session.channelID
	a.presenceMu.Unlock()
	if request == nil {
		return Errorf(ErrorCodeInvalidArgument, "presence request must not be nil")
	}
	if statusID == "" {
		id, postErr := a.postPresenceMessage(ctx, request, channelID, display)
		if postErr != nil || id == "" {
			a.logger.WarnContext(ctx, "status post failed", "component", "discord")
			return postErr
		}
		a.presenceMu.Lock()
		session.statusID = id
		session.shownText = display
		session.lastEdit = time.Now()
		session.updates = updates + 1
		a.presenceMu.Unlock()
		a.rememberPosted(session.channelKey, []string{id})
		return nil
	}
	if display == shownText {
		return nil
	}
	if patchErr := a.patchPresenceMessage(ctx, request, channelID, statusID, display); patchErr != nil {
		a.logger.WarnContext(ctx, "status edit failed", "component", "discord")
		return patchErr
	}
	a.presenceMu.Lock()
	session.shownText = display
	session.lastEdit = time.Now()
	session.updates = updates + 1
	a.presenceMu.Unlock()
	return nil
}

func (a *Adapter) stopPresence(turnID string) {
	a.presenceMu.Lock()
	session := a.presence[turnID]
	var done chan struct{}
	if session != nil {
		if session.stop != nil && !session.stopped {
			close(session.stop)
			session.stopped = true
		}
		done = session.done
	}
	a.presenceMu.Unlock()
	if done != nil {
		<-done
	}
}

func (a *Adapter) PresenceEnded(ctx context.Context, turnID string) error {
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
