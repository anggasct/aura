package telegram

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/anggasct/aura/internal/effect"
	runtimechannelhost "github.com/anggasct/aura/internal/runtime/channelhost"
)

const (
	ProviderName    = "telegram"
	OpSendMessage   = "send_message"
	messageLimit    = 4096
	sendTimeout     = 30 * time.Second
	sendMaxAttempts = 3
)

type sendOperation struct {
	ChatID  int64  `json:"chat_id"`
	Text    string `json:"text"`
	ReplyTo int64  `json:"reply_to_message_id,omitempty"`
}

type messageReceipt struct {
	ChatID    int64 `json:"chat_id"`
	MessageID int64 `json:"message_id"`
}

func (a *Adapter) Deliver(ctx context.Context, req *runtimechannelhost.DeliveryRequest) (runtimechannelhost.ProviderReceipt, error) {
	if req == nil {
		return runtimechannelhost.ProviderReceipt{}, Errorf(ErrorCodeInvalidArgument, "delivery request must not be nil")
	}
	if req.Channel == "" || req.ConversationID == "" {
		return runtimechannelhost.ProviderReceipt{}, Errorf(ErrorCodeInvalidArgument, "delivery request is missing channel or conversation")
	}
	var texts []string
	for _, part := range req.Parts {
		if strings.TrimSpace(part.Text) != "" {
			texts = append(texts, part.Text)
		}
	}
	if len(texts) == 0 {
		return runtimechannelhost.ProviderReceipt{}, Errorf(ErrorCodeInvalidArgument, "delivery request carries no text")
	}
	var target replyReference
	if err := json.Unmarshal(req.ReplyContext, &target); err != nil || target.ChatID == 0 {
		return runtimechannelhost.ProviderReceipt{}, Errorf(ErrorCodeInvalidArgument, "delivery reply context carries no chat")
	}
	select {
	case a.deliverSem <- struct{}{}:
	case <-ctx.Done():
		return runtimechannelhost.ProviderReceipt{}, ctx.Err()
	}
	defer func() { <-a.deliverSem }()
	var receipt runtimechannelhost.ProviderReceipt
	for index, chunk := range splitMessage(strings.Join(texts, "\n")) {
		operation := sendOperation{ChatID: target.ChatID, Text: chunk}
		if index == 0 {
			operation.ReplyTo = target.MessageID
		}
		messageID, err := a.executeSend(ctx, req, index, &operation)
		if err != nil {
			return runtimechannelhost.ProviderReceipt{}, err
		}
		receipt = runtimechannelhost.ProviderReceipt{ProviderID: ProviderName, ExternalID: strconv.FormatInt(messageID, 10), At: time.Now().UTC()}
	}
	return receipt, nil
}

func (a *Adapter) executeSend(ctx context.Context, req *runtimechannelhost.DeliveryRequest, index int, operation *sendOperation) (int64, error) {
	request, err := json.Marshal(operation)
	if err != nil {
		return 0, Errorf(ErrorCodeProtocolInvalid, "send request is not serializable")
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
	sum := sha256.Sum256([]byte(base + "|" + strconv.FormatInt(chatID, 10) + "|" + strconv.Itoa(index) + "|" + text))
	return ProviderName + ":" + base + ":c" + strconv.Itoa(index) + ":" + hex.EncodeToString(sum[:])[:16]
}

func receiptMessageID(intent *effect.Intent) (int64, error) {
	if intent == nil {
		return 0, Errorf(ErrorCodeDeliveryAmbiguous, "delivery outcome is unknown")
	}
	switch intent.State {
	case effect.StateSucceeded:
		var receipt messageReceipt
		if err := json.Unmarshal(intent.ProviderReceipt, &receipt); err != nil || receipt.MessageID == 0 {
			return 0, Errorf(ErrorCodeDeliveryAmbiguous, "delivery receipt is missing the message id")
		}
		return receipt.MessageID, nil
	case effect.StateUnknown:
		return 0, Errorf(ErrorCodeDeliveryAmbiguous, "delivery outcome is unknown")
	default:
		return 0, Errorf(ErrorCodeDeliveryFailed, "delivery settled without success")
	}
}

func (a *Adapter) SupportsIdempotency() bool {
	return false
}

func (a *Adapter) Invoke(ctx context.Context, invocation *effect.Invocation) (effect.Outcome, error) {
	if invocation == nil {
		return effect.Outcome{}, Errorf(ErrorCodeInvalidArgument, "invocation must not be nil")
	}
	if invocation.Operation != OpSendMessage {
		return effect.Outcome{}, Errorf(ErrorCodeInvalidArgument, "unsupported effect operation")
	}
	var operation sendOperation
	if err := json.Unmarshal(invocation.Request, &operation); err != nil {
		return effect.Outcome{}, Errorf(ErrorCodeInvalidArgument, "send request is not decodable")
	}
	if operation.ChatID == 0 || strings.TrimSpace(operation.Text) == "" {
		return effect.Outcome{}, Errorf(ErrorCodeInvalidArgument, "send request is missing chat or text")
	}
	token, err := a.tokenRef.Resolve()
	if err != nil || strings.TrimSpace(token) == "" {
		return effect.Outcome{Succeeded: false, SafeErrorCode: faultTokenUnavailable}, nil
	}
	request := &sendMessageRequest{ChatID: operation.ChatID, Text: operation.Text}
	if operation.ReplyTo != 0 {
		request.ReplyParameters = &replyParameters{MessageID: operation.ReplyTo, AllowSendingWithoutReply: true}
	}
	limited := 0
	for attempt := 0; attempt < sendMaxAttempts; attempt++ {
		call, cancel := context.WithTimeout(ctx, sendTimeout)
		sent, err := a.bot.sendMessage(call, token, request)
		cancel()
		if err == nil {
			if sent.MessageID == 0 {
				return effect.Outcome{Ambiguous: true}, nil
			}
			receipt, marshalErr := json.Marshal(messageReceipt{ChatID: operation.ChatID, MessageID: sent.MessageID})
			if marshalErr != nil {
				return effect.Outcome{}, Errorf(ErrorCodeProtocolInvalid, "receipt is not serializable")
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
			limited++
			if limited >= sendMaxAttempts {
				return effect.Outcome{Succeeded: false, SafeErrorCode: "telegram_rate_limited"}, nil
			}
			delay := apiErr.RetryAfter
			if delay <= 0 || delay > a.rateCap {
				delay = a.rateCap
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

func splitMessage(text string) []string {
	var chunks []string
	var current strings.Builder
	units := 0
	flush := func() {
		if strings.TrimSpace(current.String()) != "" {
			chunks = append(chunks, current.String())
		}
		current.Reset()
		units = 0
	}
	for _, line := range strings.SplitAfter(text, "\n") {
		if units > 0 && units+utf16Len(line) > messageLimit {
			flush()
		}
		for _, r := range line {
			width := utf16.RuneLen(r)
			if width < 1 {
				width = 1
			}
			if units+width > messageLimit {
				flush()
			}
			current.WriteRune(r)
			units += width
		}
	}
	flush()
	return chunks
}

func utf16Len(text string) int {
	n := 0
	for _, r := range text {
		width := utf16.RuneLen(r)
		if width < 1 {
			width = 1
		}
		n += width
	}
	return n
}
