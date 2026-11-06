package toolsbuiltin

import (
	"context"
	"encoding/json"
	"sync/atomic"

	"github.com/anggasct/aura/internal/approval"
	"github.com/anggasct/aura/internal/principal"
	"github.com/anggasct/aura/internal/toolbroker"
)

type EmailRoute struct {
	Account string
	Sender  string
}

type EmailMessage struct {
	SessionID      string
	IdempotencyKey string
	To             string
	Subject        string
	Body           string
}

type EmailOutbox interface {
	RouteOf(sessionID string) (EmailRoute, bool)
	Send(ctx context.Context, message *EmailMessage) (messageID string, err error)
}

type emailHolder struct {
	outbox atomic.Pointer[EmailOutbox]
}

type emailArguments struct {
	To      string `json:"to"`
	Subject string `json:"subject"`
	Body    string `json:"body"`
}

func (h *emailHolder) route(sessionID string) (EmailOutbox, EmailRoute, bool) {
	outbox := h.outbox.Load()
	if outbox == nil {
		return nil, EmailRoute{}, false
	}
	route, ok := (*outbox).RouteOf(sessionID)
	return *outbox, route, ok && route.Account != "" && route.Sender != ""
}

func (h *emailHolder) guard(_ context.Context, request *toolbroker.ToolRequest) (bool, error) {
	var args emailArguments
	if err := json.Unmarshal(request.Arguments, &args); err != nil {
		return false, err
	}
	_, route, ok := h.route(request.SessionID)
	return !ok || args.To != route.Sender, nil
}

func (h *emailHolder) adapter(ctx context.Context, request *toolbroker.ToolRequest, _ approval.Constraints) (toolbroker.ToolResult, error) {
	if request == nil {
		return toolbroker.ToolResult{}, toolbroker.Errorf(toolbroker.ResultInvalidArgument, "request must not be nil")
	}
	if request.PrincipalClass == principal.ClassPublic {
		return toolbroker.ToolResult{}, toolbroker.Errorf(toolbroker.ResultToolTierDenied, "email_send is not available to public principals")
	}
	var args emailArguments
	if err := json.Unmarshal(request.Arguments, &args); err != nil || !principal.IsPlainEmailAddress(args.To) {
		return toolbroker.ToolResult{}, toolbroker.Errorf(toolbroker.ResultInvalidArgument, "to must be one plain email address")
	}
	outbox, route, ok := h.route(request.SessionID)
	if !ok {
		return toolbroker.ToolResult{}, toolbroker.Errorf(toolbroker.ResultCapabilityUnavailable, "this session has no email account to send from")
	}
	messageID, err := outbox.Send(ctx, &EmailMessage{
		SessionID:      request.SessionID,
		IdempotencyKey: request.IdempotencyKey,
		To:             args.To,
		Subject:        args.Subject,
		Body:           args.Body,
	})
	if err != nil {
		if _, coded := toolbroker.CodeOf(err); coded {
			return toolbroker.ToolResult{}, err
		}
		return toolbroker.ToolResult{}, toolbroker.Errorf(toolbroker.ResultExecutionFailed, "email could not be sent")
	}
	encoded, err := json.Marshal(map[string]string{"status": "sent", "account": route.Account, "message_id": messageID})
	if err != nil {
		return toolbroker.ToolResult{}, err
	}
	return toolbroker.ToolResult{Class: toolbroker.ResultOK, Output: encoded}, nil
}

func (e *Executor) SetEmailOutbox(outbox EmailOutbox) {
	if e == nil || e.emails == nil || outbox == nil {
		return
	}
	e.emails.outbox.Store(&outbox)
}
