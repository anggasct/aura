package telegram

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/anggasct/aura/internal/platform/effect"
	"github.com/anggasct/aura/internal/platform/fault"
	runtimechannelhost "github.com/anggasct/aura/internal/platform/turn/channelhost"
)

const (
	ProviderName         = "telegram"
	OpSendMessage        = "send_message"
	opEditMessage        = "edit_message"
	opDeleteMessage      = "delete_message"
	messageLimit         = 4096
	sendTimeout          = 30 * time.Second
	defaultDeleteTimeout = typingTimeout
	sendMaxAttempts      = 3
)

type sendOperation struct {
	ChatID      int64           `json:"chat_id"`
	ThreadID    int64           `json:"message_thread_id,omitempty"`
	Text        string          `json:"text"`
	HTML        string          `json:"html,omitempty"`
	ReplyTo     int64           `json:"reply_to_message_id,omitempty"`
	ReplyMarkup *inlineKeyboard `json:"reply_markup,omitempty"`
	Status      bool            `json:"status,omitempty"`
	Previews    bool            `json:"previews,omitempty"`
}

type editOperation struct {
	ChatID      int64           `json:"chat_id"`
	MessageID   int64           `json:"message_id"`
	Text        string          `json:"text"`
	ReplyMarkup *inlineKeyboard `json:"reply_markup,omitempty"`
	Previews    bool            `json:"previews,omitempty"`
}

type deleteOperation struct {
	ChatID    int64 `json:"chat_id"`
	MessageID int64 `json:"message_id"`
}

type messageReceipt struct {
	ChatID    int64 `json:"chat_id"`
	MessageID int64 `json:"message_id"`
}

func (a *Adapter) Deliver(ctx context.Context, req *runtimechannelhost.DeliveryRequest) (runtimechannelhost.ProviderReceipt, error) {
	if req == nil {
		return runtimechannelhost.ProviderReceipt{}, fault.New(ErrorCodeInvalidArgument, "delivery request must not be nil")
	}
	return a.deliver(ctx, req, !req.Tainted)
}

func (a *Adapter) deliver(ctx context.Context, req *runtimechannelhost.DeliveryRequest, previews bool) (runtimechannelhost.ProviderReceipt, error) {
	if req == nil {
		return runtimechannelhost.ProviderReceipt{}, fault.New(ErrorCodeInvalidArgument, "delivery request must not be nil")
	}
	if req.Channel == "" || req.ConversationID == "" {
		return runtimechannelhost.ProviderReceipt{}, fault.New(ErrorCodeInvalidArgument, "delivery request is missing channel or conversation")
	}
	var texts []string
	for _, part := range req.Parts {
		if strings.TrimSpace(part.Text) != "" {
			texts = append(texts, part.Text)
		}
	}
	if len(texts) == 0 {
		return runtimechannelhost.ProviderReceipt{}, fault.New(ErrorCodeInvalidArgument, "delivery request carries no text")
	}
	var target replyReference
	if err := json.Unmarshal(req.ReplyContext, &target); err != nil || target.ChatID == 0 {
		return runtimechannelhost.ProviderReceipt{}, fault.New(ErrorCodeInvalidArgument, "delivery reply context carries no chat")
	}
	if req.EffectID != "" {
		a.stopPresence(req.EffectID)
		a.clearPresenceStop(ctx, req.EffectID)
	}
	select {
	case a.deliverSem <- struct{}{}:
	case <-ctx.Done():
		return runtimechannelhost.ProviderReceipt{}, ctx.Err()
	}
	defer func() { <-a.deliverSem }()
	var receipt runtimechannelhost.ProviderReceipt
	quote := a.shouldQuote(&target)
	for index, chunk := range splitMessage(strings.Join(texts, "\n")) {
		operation := sendOperation{ChatID: target.ChatID, ThreadID: target.ThreadID, Text: chunk, HTML: renderHTML(chunk), Previews: previews}
		if index == 0 && quote {
			operation.ReplyTo = target.MessageID
		}
		messageID, err := a.executeSend(ctx, req, index, &operation)
		if err != nil {
			return runtimechannelhost.ProviderReceipt{}, err
		}
		receipt = runtimechannelhost.ProviderReceipt{ProviderID: ProviderName, ExternalID: strconv.FormatInt(messageID, 10), At: time.Now().UTC()}
	}
	if req.EffectID != "" {
		a.deleteStatus(ctx, req)
	}
	return receipt, nil
}

func (a *Adapter) shouldQuote(target *replyReference) bool {
	if target.MessageID == 0 {
		return false
	}
	if target.ChatType != chatTypePrivate {
		return true
	}
	return a.latestPrivate.hasNewer(target.ChatID, target.FromID, target.MessageID)
}

func (a *Adapter) deleteStatus(ctx context.Context, req *runtimechannelhost.DeliveryRequest) {
	chatID, statusID, ok := a.takeStatusToDelete(req.EffectID)
	if !ok {
		return
	}
	request, err := json.Marshal(deleteOperation{ChatID: chatID, MessageID: statusID})
	if err != nil {
		a.logger.WarnContext(ctx, "status delete failed", "component", "telegram", "error", err)
		return
	}
	intent, err := a.effects.Execute(ctx, &effect.PrepareRequest{
		SessionID:      req.ConversationID,
		IdempotencyKey: deleteKey(req.EffectID, statusID),
		Provider:       ProviderName,
		Operation:      opDeleteMessage,
		Classification: effect.ClassificationEffectful,
		Request:        request,
		EventKind:      effect.EventKindChannelRequested,
	}, a)
	if err != nil {
		a.logger.WarnContext(ctx, "status delete failed", "component", "telegram", "error", err)
		return
	}
	if intent == nil || intent.State != effect.StateSucceeded {
		a.logger.WarnContext(ctx, "status delete failed", "component", "telegram")
	}
}

func (a *Adapter) executeSend(ctx context.Context, req *runtimechannelhost.DeliveryRequest, index int, operation *sendOperation) (int64, error) {
	request, err := json.Marshal(operation)
	if err != nil {
		return 0, fault.New(ErrorCodeProtocolInvalid, "send request is not serializable")
	}
	intent, err := a.effects.Execute(ctx, &effect.PrepareRequest{
		SessionID:      req.ConversationID,
		IdempotencyKey: idempotencyKey(keyBase(req), operation.ChatID, index, operation.Text),
		Provider:       ProviderName,
		Operation:      OpSendMessage,
		Classification: effect.ClassificationEffectful,
		Request:        request,
		EventKind:      effect.EventKindChannelRequested,
	}, a)
	if err != nil {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		if intent != nil {
			if _, stateErr := receiptMessageID(intent); stateErr != nil {
				return 0, stateErr
			}
		}
		return 0, err
	}
	return receiptMessageID(intent)
}

func keyBase(req *runtimechannelhost.DeliveryRequest) string {
	if req.EffectID != "" {
		return req.EffectID
	}
	return "anon:" + req.IdempotencyKey
}

func idempotencyKey(base string, chatID int64, index int, text string) string {
	return effectKey(base, chatID, "c"+strconv.Itoa(index), strconv.Itoa(index), text)
}

func statusKey(base string, chatID int64, slot, text string) string {
	return effectKey(base, chatID, slot, slot, text)
}

func deleteKey(turnID string, messageID int64) string {
	sum := sha256.Sum256([]byte(strconv.FormatInt(messageID, 10)))
	return ProviderName + ":" + turnID + ":d:" + hex.EncodeToString(sum[:])[:16]
}

func effectKey(base string, chatID int64, slot, digestSlot, text string) string {
	sum := sha256.Sum256([]byte(base + "|" + strconv.FormatInt(chatID, 10) + "|" + digestSlot + "|" + text))
	return ProviderName + ":" + base + ":" + slot + ":" + hex.EncodeToString(sum[:])[:16]
}

func receiptMessageID(intent *effect.Intent) (int64, error) {
	if intent == nil {
		return 0, deliveryAmbiguous("delivery outcome is unknown")
	}
	switch intent.State {
	case effect.StateSucceeded:
		var receipt messageReceipt
		if err := json.Unmarshal(intent.ProviderReceipt, &receipt); err != nil || receipt.MessageID == 0 {
			return 0, deliveryAmbiguous("delivery receipt is missing the message id")
		}
		return receipt.MessageID, nil
	case effect.StateUnknown, effect.StateStarted:
		return 0, deliveryAmbiguous("delivery outcome is unknown")
	default:
		return 0, fault.New(ErrorCodeDeliveryFailed, "delivery settled without success")
	}
}

func (a *Adapter) SupportsIdempotency() bool {
	return false
}

func (a *Adapter) Invoke(ctx context.Context, invocation *effect.Invocation) (effect.Outcome, error) {
	if invocation == nil {
		return effect.Outcome{}, fault.New(ErrorCodeInvalidArgument, "invocation must not be nil")
	}
	switch invocation.Operation {
	case OpSendMessage:
		return a.invokeSend(ctx, invocation.Request)
	case opEditMessage:
		return a.invokeEdit(ctx, invocation.Request)
	case opDeleteMessage:
		return a.invokeDelete(ctx, invocation.Request)
	default:
		return effect.Outcome{}, fault.New(ErrorCodeInvalidArgument, "unsupported effect operation")
	}
}

func (a *Adapter) invokeSend(ctx context.Context, raw json.RawMessage) (effect.Outcome, error) {
	var operation sendOperation
	if err := json.Unmarshal(raw, &operation); err != nil {
		return effect.Outcome{}, fault.New(ErrorCodeInvalidArgument, "send request is not decodable")
	}
	if operation.ChatID == 0 || strings.TrimSpace(operation.Text) == "" {
		return effect.Outcome{}, fault.New(ErrorCodeInvalidArgument, "send request is missing chat or text")
	}
	request := &sendMessageRequest{ChatID: operation.ChatID, MessageThreadID: operation.ThreadID, Text: operation.Text, ReplyMarkup: operation.ReplyMarkup, LinkPreviewOptions: previewsDisabledUnlessOptedIn(a.account.LinkPreviews && operation.Previews)}
	if operation.HTML != "" {
		request.Text = operation.HTML
		request.ParseMode = parseModeHTML
	}
	if operation.ReplyTo != 0 {
		request.ReplyParameters = &replyParameters{MessageID: operation.ReplyTo, AllowSendingWithoutReply: true}
	}
	return a.dispatch(ctx, operation.ChatID, !operation.Status, func(call context.Context, token string) (int64, error) {
		sent, err := a.bot.sendMessage(call, token, request)
		if apiErr, rejected := apiErrorOf(err); rejected && apiErr.Status == http.StatusBadRequest && request.ParseMode != "" {
			request.Text = operation.Text
			request.ParseMode = ""
			sent, err = a.bot.sendMessage(call, token, request)
		}
		return sent.MessageID, err
	})
}

func (a *Adapter) invokeEdit(ctx context.Context, raw json.RawMessage) (effect.Outcome, error) {
	var operation editOperation
	if err := json.Unmarshal(raw, &operation); err != nil {
		return effect.Outcome{}, fault.New(ErrorCodeInvalidArgument, "edit request is not decodable")
	}
	if operation.ChatID == 0 || operation.MessageID == 0 || strings.TrimSpace(operation.Text) == "" {
		return effect.Outcome{}, fault.New(ErrorCodeInvalidArgument, "edit request is missing chat, message, or text")
	}
	request := &editMessageTextRequest{ChatID: operation.ChatID, MessageID: operation.MessageID, Text: operation.Text, ReplyMarkup: operation.ReplyMarkup, LinkPreviewOptions: previewsDisabledUnlessOptedIn(a.account.LinkPreviews && operation.Previews)}
	return a.dispatch(ctx, operation.ChatID, false, func(call context.Context, token string) (int64, error) {
		edited, err := a.bot.editMessageText(call, token, request)
		if err != nil {
			return 0, err
		}
		return cmp.Or(edited.MessageID, operation.MessageID), nil
	})
}

func (a *Adapter) invokeDelete(ctx context.Context, raw json.RawMessage) (effect.Outcome, error) {
	var operation deleteOperation
	if err := json.Unmarshal(raw, &operation); err != nil {
		return effect.Outcome{}, fault.New(ErrorCodeInvalidArgument, "delete request is not decodable")
	}
	if operation.ChatID == 0 || operation.MessageID == 0 {
		return effect.Outcome{}, fault.New(ErrorCodeInvalidArgument, "delete request is missing chat or message")
	}
	return a.dispatchWithTimeout(ctx, operation.ChatID, false, a.deleteTimeoutOrDefault(), func(call context.Context, token string) (int64, error) {
		err := a.bot.deleteMessage(call, token, operation.ChatID, operation.MessageID)
		if apiErr, rejected := apiErrorOf(err); rejected && apiErr.Status == http.StatusBadRequest && apiErr.DeleteNotFound {
			return operation.MessageID, nil
		}
		if err != nil {
			return 0, err
		}
		return operation.MessageID, nil
	})
}

func (a *Adapter) deleteTimeoutOrDefault() time.Duration {
	if a.deleteTimeout > 0 {
		return a.deleteTimeout
	}
	return defaultDeleteTimeout
}

func (a *Adapter) dispatch(ctx context.Context, chatID int64, waitOnLimit bool, send func(ctx context.Context, token string) (int64, error)) (effect.Outcome, error) {
	return a.dispatchWithTimeout(ctx, chatID, waitOnLimit, sendTimeout, send)
}

func (a *Adapter) dispatchWithTimeout(ctx context.Context, chatID int64, waitOnLimit bool, timeout time.Duration, send func(ctx context.Context, token string) (int64, error)) (effect.Outcome, error) {
	token, ok := a.botToken()
	if !ok {
		return effect.Outcome{Succeeded: false, SafeErrorCode: faultBotRefUnresolved}, nil
	}
	limited := 0
	for range sendMaxAttempts {
		call, cancel := context.WithTimeout(ctx, timeout)
		messageID, err := send(call, token)
		cancel()
		if err == nil {
			if messageID == 0 {
				return effect.Outcome{Ambiguous: true}, nil
			}
			receipt, marshalErr := json.Marshal(messageReceipt{ChatID: chatID, MessageID: messageID})
			if marshalErr != nil {
				return effect.Outcome{}, fault.New(ErrorCodeProtocolInvalid, "receipt is not serializable")
			}
			return effect.Outcome{Succeeded: true, Receipt: receipt}, nil
		}
		if ctx.Err() != nil {
			return effect.Outcome{}, ctx.Err()
		}
		apiErr, rejected := apiErrorOf(err)
		switch {
		case !rejected && isDialError(err):
			continue
		case !rejected:
			return effect.Outcome{Ambiguous: true}, nil
		case apiErr.Status == http.StatusTooManyRequests:
			delay := apiErr.RetryAfter
			if delay <= 0 || delay > a.rateCap {
				delay = a.rateCap
			}
			a.holdChat(chatID, delay)
			limited++
			if !waitOnLimit || limited >= sendMaxAttempts {
				return effect.Outcome{Succeeded: false, SafeErrorCode: "telegram_rate_limited"}, nil
			}
			if !sleepContext(ctx, delay) {
				return effect.Outcome{}, ctx.Err()
			}
		case apiErr.Status == http.StatusUnauthorized:
			return effect.Outcome{Succeeded: false, SafeErrorCode: "telegram_unauthorized"}, nil
		case apiErr.Status == http.StatusForbidden:
			return effect.Outcome{Succeeded: false, SafeErrorCode: "telegram_forbidden"}, nil
		case apiErr.Status >= 400 && apiErr.Status < 500:
			return effect.Outcome{Succeeded: false, SafeErrorCode: "telegram_rejected"}, nil
		default:
			return effect.Outcome{Ambiguous: true}, nil
		}
	}
	return effect.Outcome{Ambiguous: true}, nil
}
