package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	botAPIBase       = "https://api.telegram.org"
	maxResponseBytes = 4 << 20
)

type apiResponse struct {
	OK          bool            `json:"ok"`
	Result      json.RawMessage `json:"result"`
	ErrorCode   int             `json:"error_code"`
	Description string          `json:"description"`
	Parameters  *struct {
		RetryAfter int `json:"retry_after"`
	} `json:"parameters"`
}

type APIError struct {
	Method     string
	Status     int
	RetryAfter time.Duration
}

func (e *APIError) Error() string {
	return fmt.Sprintf("telegram bot api %s returned status %d", e.Method, e.Status)
}

type redactedError struct {
	text  string
	cause error
}

func (e *redactedError) Error() string {
	return e.text
}

func (e *redactedError) Unwrap() error {
	return e.cause
}

type botClient struct {
	base string
	http *http.Client
}

func newBotClient() *botClient {
	return &botClient{
		base: botAPIBase,
		http: &http.Client{
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

func (c *botClient) call(ctx context.Context, token, method string, payload, result any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return Errorf(ErrorCodeProtocolInvalid, "bot api %s payload is not serializable", method)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/bot"+token+"/"+method, bytes.NewReader(body))
	if err != nil {
		return Errorf(ErrorCodeConnectionFailed, "bot api %s request is not constructible", method)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return &Error{Code: ErrorCodeConnectionFailed, Detail: "bot api " + method + " request failed", Cause: redact(err, token)}
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return &Error{Code: ErrorCodeConnectionFailed, Detail: "bot api " + method + " response is not readable", Cause: redact(err, token)}
	}
	if len(raw) > maxResponseBytes {
		return Errorf(ErrorCodeProtocolInvalid, "bot api %s response exceeds the size bound", method)
	}
	var envelope apiResponse
	if err := json.Unmarshal(raw, &envelope); err != nil {
		if resp.StatusCode != http.StatusOK {
			return &Error{Code: ErrorCodeBotAPIRejected, Detail: "bot api " + method + " failed", Cause: &APIError{Method: method, Status: resp.StatusCode}}
		}
		return Errorf(ErrorCodeProtocolInvalid, "bot api %s response is not decodable", method)
	}
	if !envelope.OK || resp.StatusCode != http.StatusOK {
		apiErr := &APIError{Method: method, Status: envelope.ErrorCode}
		if apiErr.Status == 0 {
			apiErr.Status = resp.StatusCode
		}
		if envelope.Parameters != nil && envelope.Parameters.RetryAfter > 0 {
			apiErr.RetryAfter = time.Duration(envelope.Parameters.RetryAfter) * time.Second
		}
		return &Error{Code: ErrorCodeBotAPIRejected, Detail: "bot api " + method + " failed", Cause: apiErr}
	}
	if result == nil {
		return nil
	}
	if err := json.Unmarshal(envelope.Result, result); err != nil {
		return Errorf(ErrorCodeProtocolInvalid, "bot api %s result is not decodable", method)
	}
	return nil
}

func redact(err error, token string) error {
	cause := err
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		cause = urlErr.Err
	}
	text := cause.Error()
	if token != "" {
		text = strings.ReplaceAll(text, token, "[redacted]")
	}
	return &redactedError{text: text, cause: cause}
}

func apiErrorOf(err error) (*APIError, bool) {
	var target *APIError
	if !errors.As(err, &target) {
		return nil, false
	}
	return target, true
}

func isDialError(err error) bool {
	var opErr *net.OpError
	return errors.As(err, &opErr) && opErr.Op == "dial"
}

type update struct {
	UpdateID int64    `json:"update_id"`
	Message  *message `json:"message"`
}

type message struct {
	MessageID int64  `json:"message_id"`
	From      *user  `json:"from"`
	Chat      chat   `json:"chat"`
	Text      string `json:"text"`
}

type user struct {
	ID    int64 `json:"id"`
	IsBot bool  `json:"is_bot"`
}

type chat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}

type getUpdatesRequest struct {
	Offset         int64    `json:"offset,omitempty"`
	Timeout        int      `json:"timeout"`
	AllowedUpdates []string `json:"allowed_updates"`
}

type replyParameters struct {
	MessageID                int64 `json:"message_id"`
	AllowSendingWithoutReply bool  `json:"allow_sending_without_reply"`
}

type sendMessageRequest struct {
	ChatID          int64            `json:"chat_id"`
	Text            string           `json:"text"`
	ReplyParameters *replyParameters `json:"reply_parameters,omitempty"`
}

type sentMessage struct {
	MessageID int64 `json:"message_id"`
}

func (c *botClient) getUpdates(ctx context.Context, token string, offset int64, timeout time.Duration) ([]update, error) {
	var updates []update
	err := c.call(ctx, token, "getUpdates", getUpdatesRequest{
		Offset:         offset,
		Timeout:        int(timeout / time.Second),
		AllowedUpdates: []string{"message"},
	}, &updates)
	return updates, err
}

func (c *botClient) sendMessage(ctx context.Context, token string, req *sendMessageRequest) (sentMessage, error) {
	var sent sentMessage
	err := c.call(ctx, token, "sendMessage", req, &sent)
	return sent, err
}
