package discord

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/anggasct/aura/internal/effect"
)

const (
	opSendMessage = "send_message"
	opEditMessage = "edit_message"
)

const (
	discordMessageLimit = 2000
	maxTextMessages     = 3
	sendMaxAttempts     = 3
	sendTimeout         = 30 * time.Second
	rateLimitCap        = time.Minute
	maxRestErrorBytes   = 1 << 16
)

type EffectRunner interface {
	Execute(ctx context.Context, req *effect.PrepareRequest, provider effect.Provider) (*effect.Intent, error)
}

type sendOperation struct {
	ChannelID string `json:"channel_id"`
	Text      string `json:"text"`
	ReplyTo   string `json:"reply_to_message_id,omitempty"`
	FileName  string `json:"file_name,omitempty"`
	FileBody  []byte `json:"file_body,omitempty"`
}

type editOperation struct {
	ChannelID string `json:"channel_id"`
	MessageID string `json:"message_id"`
	Text      string `json:"text"`
}

type messageReceipt struct {
	MessageID string `json:"message_id"`
	ChannelID string `json:"channel_id"`
}

func neutralizeMentions(text string) string {
	replacer := strings.NewReplacer("@everyone", "@\u200beveryone", "@here", "@\u200bhere")
	return replacer.Replace(text)
}

func splitMessage(text string) []string {
	if len([]rune(text)) <= discordMessageLimit {
		return []string{text}
	}
	var chunks []string
	var current strings.Builder
	currentRunes := 0
	flush := func() {
		if currentRunes > 0 {
			chunks = append(chunks, current.String())
			current.Reset()
			currentRunes = 0
		}
	}
	for _, line := range strings.SplitAfter(text, "\n") {
		runes := []rune(line)
		for len(runes) > 0 {
			room := discordMessageLimit - currentRunes
			if room <= 0 {
				flush()
				room = discordMessageLimit
			}
			take := room
			if len(runes) < take {
				take = len(runes)
			}
			current.WriteString(string(runes[:take]))
			currentRunes += take
			runes = runes[take:]
		}
	}
	flush()
	return chunks
}

func idempotencyKey(effectID, chunk, text string) string {
	sum := sha256.Sum256([]byte(effectID + "|" + chunk + "|" + text))
	return "discord:" + effectID + ":" + chunk + ":" + hex.EncodeToString(sum[:])[:16]
}

func (a *Adapter) SupportsIdempotency() bool {
	return false
}

func (a *Adapter) Invoke(ctx context.Context, invocation *effect.Invocation) (effect.Outcome, error) {
	if invocation == nil {
		return effect.Outcome{}, Errorf(ErrorCodeInvalidArgument, "invocation must not be nil")
	}
	switch invocation.Operation {
	case opSendMessage:
		return a.invokeSend(ctx, invocation)
	case opEditMessage:
		return a.invokeEdit(ctx, invocation)
	default:
		return effect.Outcome{}, Errorf(ErrorCodeInvalidArgument, "unsupported effect operation")
	}
}

func (a *Adapter) invokeSend(ctx context.Context, invocation *effect.Invocation) (effect.Outcome, error) {
	var operation sendOperation
	if err := json.Unmarshal(invocation.Request, &operation); err != nil {
		return effect.Outcome{}, Errorf(ErrorCodeInvalidArgument, "send request is not decodable")
	}
	if operation.ChannelID == "" || operation.Text == "" {
		return effect.Outcome{}, Errorf(ErrorCodeInvalidArgument, "send request is missing channel or text")
	}
	token, err := a.tokenRef.Resolve()
	if err != nil {
		return effect.Outcome{}, Errorf(ErrorCodeConnectionFailed, "bot token is unavailable")
	}
	messageID, outcome, err := a.postMessage(ctx, token, &operation)
	if err != nil {
		return effect.Outcome{}, err
	}
	if !outcome.Succeeded {
		return outcome, nil
	}
	receipt, err := json.Marshal(messageReceipt{MessageID: messageID, ChannelID: operation.ChannelID})
	if err != nil {
		return effect.Outcome{}, Errorf(ErrorCodeProtocolInvalid, "receipt is not serializable")
	}
	outcome.Receipt = receipt
	return outcome, nil
}

func (a *Adapter) invokeEdit(ctx context.Context, invocation *effect.Invocation) (effect.Outcome, error) {
	var operation editOperation
	if err := json.Unmarshal(invocation.Request, &operation); err != nil {
		return effect.Outcome{}, Errorf(ErrorCodeInvalidArgument, "edit request is not decodable")
	}
	if operation.ChannelID == "" || operation.MessageID == "" || operation.Text == "" {
		return effect.Outcome{}, Errorf(ErrorCodeInvalidArgument, "edit request is missing channel, message, or text")
	}
	token, err := a.tokenRef.Resolve()
	if err != nil {
		return effect.Outcome{}, Errorf(ErrorCodeConnectionFailed, "bot token is unavailable")
	}
	outcome, err := a.patchMessage(ctx, token, &operation)
	if err != nil {
		return effect.Outcome{}, err
	}
	if !outcome.Succeeded {
		return outcome, nil
	}
	receipt, err := json.Marshal(messageReceipt{MessageID: operation.MessageID, ChannelID: operation.ChannelID})
	if err != nil {
		return effect.Outcome{}, Errorf(ErrorCodeProtocolInvalid, "receipt is not serializable")
	}
	outcome.Receipt = receipt
	return outcome, nil
}

func restDeadline(ctx context.Context, deadline time.Time) (context.Context, context.CancelFunc) {
	timeout := time.Now().Add(sendTimeout)
	if !deadline.IsZero() && deadline.Before(timeout) {
		timeout = deadline
	}
	return context.WithDeadline(ctx, timeout)
}

func (a *Adapter) postMessage(ctx context.Context, token string, operation *sendOperation) (string, effect.Outcome, error) {
	body, contentType, err := sendBody(operation)
	if err != nil {
		return "", effect.Outcome{}, err
	}
	endpoint := a.restBase + "/channels/" + operation.ChannelID + "/messages"
	response, outcome, err := a.requestWithRetry(ctx, http.MethodPost, endpoint, token, body, contentType)
	if err != nil || !outcome.Succeeded {
		return "", outcome, err
	}
	var message struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(response, &message)
	if message.ID == "" {
		return "", effect.Outcome{Ambiguous: true}, nil
	}
	return message.ID, outcome, nil
}

func (a *Adapter) patchMessage(ctx context.Context, token string, operation *editOperation) (effect.Outcome, error) {
	payload, err := json.Marshal(map[string]any{
		"content":          operation.Text,
		"allowed_mentions": map[string]any{"parse": []string{}},
	})
	if err != nil {
		return effect.Outcome{}, Errorf(ErrorCodeProtocolInvalid, "edit payload is not serializable")
	}
	endpoint := a.restBase + "/channels/" + operation.ChannelID + "/messages/" + operation.MessageID
	_, outcome, err := a.requestWithRetry(ctx, http.MethodPatch, endpoint, token, payload, "application/json")
	return outcome, err
}

func (a *Adapter) requestWithRetry(ctx context.Context, method, endpoint, token string, body []byte, contentType string) ([]byte, effect.Outcome, error) {
	limited := 0
	for range sendMaxAttempts {
		status, response, retryAfter, err := a.restCall(ctx, method, endpoint, token, body, contentType)
		if err != nil {
			if isDialError(err) {
				continue
			}
			return nil, effect.Outcome{Ambiguous: true}, nil
		}
		switch {
		case status >= 200 && status < 300:
			return response, effect.Outcome{Succeeded: true}, nil
		case status == http.StatusTooManyRequests:
			limited++
			if limited >= sendMaxAttempts {
				return nil, effect.Outcome{Ambiguous: true}, nil
			}
			delay := retryAfter
			if delay <= 0 || delay > a.rateCap {
				delay = a.rateCap
			}
			if !sleepContext(ctx, delay) {
				return nil, effect.Outcome{}, ctx.Err()
			}
		case status == http.StatusUnauthorized || status == http.StatusForbidden:
			return nil, effect.Outcome{Succeeded: false, SafeErrorCode: "discord_unauthorized"}, nil
		case status == http.StatusNotFound:
			return nil, effect.Outcome{Succeeded: false, SafeErrorCode: "discord_not_found"}, nil
		case status >= 400 && status < 500:
			return nil, effect.Outcome{Succeeded: false, SafeErrorCode: "discord_rejected"}, nil
		default:
			return nil, effect.Outcome{Ambiguous: true}, nil
		}
	}
	return nil, effect.Outcome{Ambiguous: true}, nil
}

func isDialError(err error) bool {
	var opErr *net.OpError
	return errors.As(err, &opErr) && opErr.Op == "dial"
}

func sendBody(operation *sendOperation) (body []byte, contentType string, err error) {
	message := map[string]any{
		"content":          operation.Text,
		"allowed_mentions": map[string]any{"parse": []string{}},
	}
	if operation.ReplyTo != "" {
		message["message_reference"] = map[string]any{"message_id": operation.ReplyTo}
	}
	if len(operation.FileBody) == 0 {
		raw, err := json.Marshal(message)
		if err != nil {
			return nil, "", Errorf(ErrorCodeProtocolInvalid, "send payload is not serializable")
		}
		return raw, "application/json", nil
	}
	var upload bytes.Buffer
	writer := multipart.NewWriter(&upload)
	payload, err := json.Marshal(message)
	if err != nil {
		return nil, "", Errorf(ErrorCodeProtocolInvalid, "send payload is not serializable")
	}
	if err := writer.WriteField("payload_json", string(payload)); err != nil {
		return nil, "", Errorf(ErrorCodeProtocolInvalid, "send payload is not serializable")
	}
	part, err := writer.CreateFormFile("files[0]", operation.FileName)
	if err != nil {
		return nil, "", Errorf(ErrorCodeProtocolInvalid, "upload part is not writable")
	}
	if _, err := part.Write(operation.FileBody); err != nil {
		return nil, "", Errorf(ErrorCodeProtocolInvalid, "upload part is not writable")
	}
	if err := writer.Close(); err != nil {
		return nil, "", Errorf(ErrorCodeProtocolInvalid, "upload body is not serializable")
	}
	return upload.Bytes(), writer.FormDataContentType(), nil
}

func (a *Adapter) restCall(ctx context.Context, method, endpoint, token string, body []byte, contentType string) (status int, response []byte, retryAfter time.Duration, err error) {
	call, cancel := restDeadline(ctx, time.Time{})
	defer cancel()
	req, err := http.NewRequestWithContext(call, method, endpoint, bytes.NewReader(body))
	if err != nil {
		return 0, nil, 0, err
	}
	req.Header.Set("Authorization", "Bot "+token)
	req.Header.Set("Content-Type", contentType)
	resp, err := a.httpClient.Do(req)
	if err != nil {
		return 0, nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	read, readErr := io.ReadAll(io.LimitReader(resp.Body, maxRestErrorBytes))
	if readErr != nil {
		return 0, nil, 0, readErr
	}
	response = read
	if resp.StatusCode == http.StatusTooManyRequests {
		return resp.StatusCode, nil, parseRetryAfter(resp.Header.Get("Retry-After")), nil
	}
	return resp.StatusCode, response, 0, nil
}

func parseRetryAfter(raw string) time.Duration {
	seconds, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil || seconds <= 0 {
		return 0
	}
	return time.Duration(seconds * float64(time.Second))
}

type uploadChunk struct {
	name string
	text string
}

func splitUploads(chunks []string, fileCap int64) ([]string, []uploadChunk, error) {
	if len(chunks) <= maxTextMessages {
		return chunks, nil, nil
	}
	texts := chunks[:maxTextMessages]
	remainder := strings.Join(chunks[maxTextMessages:], "")
	if int64(len(remainder)) > fileCap {
		return nil, nil, Errorf(ErrorCodeMessageTooLarge, "message exceeds the text and file budget")
	}
	return texts, []uploadChunk{{name: "message.txt", text: remainder}}, nil
}

func parseReplyContext(raw json.RawMessage, channel string) (channelID, messageID string, err error) {
	if len(raw) == 0 {
		if !isChannelID(channel) {
			return "", "", Errorf(ErrorCodeInvalidArgument, "delivery channel is not a discord channel id")
		}
		return channel, "", nil
	}
	var reference replyReference
	if err := json.Unmarshal(raw, &reference); err != nil {
		return "", "", Errorf(ErrorCodeInvalidArgument, "reply context is not decodable")
	}
	if !isChannelID(reference.ChannelID) {
		return "", "", Errorf(ErrorCodeInvalidArgument, "reply context carries no channel id")
	}
	return reference.ChannelID, reference.MessageID, nil
}

func isChannelID(id string) bool {
	if id == "" {
		return false
	}
	for i := range len(id) {
		if id[i] < '0' || id[i] > '9' {
			return false
		}
	}
	return true
}

func marshalOperation(operation any) (json.RawMessage, error) {
	raw, err := json.Marshal(operation)
	if err != nil {
		return nil, Errorf(ErrorCodeProtocolInvalid, "effect request is not serializable")
	}
	return raw, nil
}

func unmarshalReceipt(raw json.RawMessage, receipt *messageReceipt) error {
	if len(raw) == 0 {
		return Errorf(ErrorCodeDeliveryAmbiguous, "delivery receipt is empty")
	}
	return json.Unmarshal(raw, receipt)
}
