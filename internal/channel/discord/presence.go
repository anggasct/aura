package discord

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
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
	started        time.Time
	lastTyping     time.Time
	statusID       string
	lastText       string
	lastEdit       time.Time
	updates        int
	failedNotified bool
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
	channelID, _, err := parseReplyContext(req.ReplyContext, req.Channel)
	if err != nil {
		return nil, "", err
	}
	a.presenceMu.Lock()
	defer a.presenceMu.Unlock()
	if a.presence == nil {
		a.presence = map[string]*presenceSession{}
	}
	session, ok := a.presence[req.EffectID]
	if !ok {
		now := time.Now()
		session = &presenceSession{
			channelID:    channelID,
			conversation: req.ConversationID,
			channelKey:   presenceKey(req.Channel, req.EffectID),
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
	a.presenceMu.Unlock()
	if err := a.triggerTyping(context.WithoutCancel(ctx), channelID); err != nil {
		a.logger.WarnContext(ctx, "typing trigger failed", "component", "discord")
	}
	return nil
}

func (a *Adapter) PresenceUpdated(ctx context.Context, req *runtimechannelhost.DeliveryRequest, text string, failed bool) error {
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
	lastEdit := session.lastEdit
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
		a.presenceMu.Unlock()
		if err := a.postOrPatchStatus(ctx, req, channelID, session, display, updates, statusID); err != nil {
			return err
		}
		a.presenceMu.Lock()
		session.lastText = display
		a.presenceMu.Unlock()
		return nil
	}
	if strings.TrimSpace(text) == "" {
		text = lastText
	}
	display := renderStatusText(text, "", updates, a.vaultOrNil())
	if display == "" {
		return nil
	}
	now := time.Now()
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
	if statusID != "" && now.Sub(lastEdit) < time.Duration(a.cfg.MinEditInterval) {
		a.presenceMu.Lock()
		session.lastText = display
		session.updates = updates + 1
		session.failedNotified = false
		a.presenceMu.Unlock()
		return nil
	}
	a.presenceMu.Lock()
	session.failedNotified = false
	a.presenceMu.Unlock()
	if err := a.postOrPatchStatus(ctx, req, channelID, session, display, updates, statusID); err != nil {
		return err
	}
	a.presenceMu.Lock()
	session.lastText = display
	a.presenceMu.Unlock()
	return nil
}

func (a *Adapter) postOrPatchStatus(ctx context.Context, req *runtimechannelhost.DeliveryRequest, channelID string, session *presenceSession, display string, updates int, statusID string) error {
	if statusID == "" {
		id, postErr := a.postPresenceMessage(ctx, req, channelID, display)
		if postErr != nil {
			a.logger.WarnContext(ctx, "status post failed", "component", "discord")
		}
		a.presenceMu.Lock()
		session.statusID = id
		session.lastEdit = time.Now()
		session.updates = updates + 1
		a.presenceMu.Unlock()
		a.rememberPosted(session.channelKey, []string{id})
		return nil
	}
	if display == session.lastText {
		return nil
	}
	if patchErr := a.patchPresenceMessage(ctx, req, channelID, statusID, display); patchErr != nil {
		a.logger.WarnContext(ctx, "status edit failed", "component", "discord")
	}
	a.presenceMu.Lock()
	session.lastEdit = time.Now()
	session.updates = updates + 1
	a.presenceMu.Unlock()
	return nil
}

func (a *Adapter) PresenceEnded(ctx context.Context, turnID string) error {
	if turnID == "" {
		return Errorf(ErrorCodeInvalidArgument, "presence turn id must not be empty")
	}
	a.presenceMu.Lock()
	if a.presence != nil {
		delete(a.presence, turnID)
	}
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
		IdempotencyKey: idempotencyKey(req.EffectID, "s0", text),
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
		IdempotencyKey: idempotencyKey(req.EffectID, "se0", text),
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
