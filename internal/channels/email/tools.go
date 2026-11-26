package email

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/toolbroker"
	"github.com/anggasct/aura/internal/platform/tools"
)

const (
	SendToolName    = "email_send"
	MaxSubjectBytes = 160
	MaxBodyBytes    = 65536
)

type outbox interface {
	RouteOf(sessionID string) (Route, bool)
	Send(ctx context.Context, outgoing *Outgoing) (string, error)
}

type sendArguments struct {
	To      string `json:"to"`
	Subject string `json:"subject"`
	Body    string `json:"body"`
}

func SendToolDefinition() tools.Definition {
	return tools.Definition{
		Name:      SendToolName,
		Version:   tools.Version,
		Channel:   principal.ChannelEmail,
		Schema:    []byte(`{"$schema":"https://json-schema.org/draft/2020-12/schema","$id":"aura://tools/email_send/v1","type":"object","properties":{"to":{"type":"string","minLength":3,"maxLength":254},"subject":{"type":"string","minLength":1,"maxLength":160},"body":{"type":"string","minLength":1,"maxLength":65536}},"required":["to","subject","body"],"additionalProperties":false}`),
		Effectful: true,
		OptIn:     true,
		Display:   []string{"to", "subject", "body"},
		Validator: validateSendArguments,
	}
}

func SendToolRule() approval.Rule {
	definition := SendToolDefinition()
	return toolbroker.DefaultRule(&definition)
}

func validateSendArguments(raw json.RawMessage) (json.RawMessage, error) {
	value, err := tools.DecodeObject(raw)
	if err != nil {
		return nil, err
	}
	if err := tools.RejectUnknown(value, "to", "subject", "body"); err != nil {
		return nil, err
	}
	for _, key := range []string{"to", "subject", "body"} {
		if err := tools.ValidateString(value, key); err != nil {
			return nil, err
		}
	}
	rawTo, _ := value["to"].(string)
	subject, _ := value["subject"].(string)
	body, _ := value["body"].(string)
	to := strings.ToLower(strings.TrimSpace(rawTo))
	if !principal.IsPlainEmailAddress(to) {
		return nil, fmt.Errorf("argument %q must be exactly one plain email address", "to")
	}
	value["to"] = to
	if len(subject) > MaxSubjectBytes {
		return nil, fmt.Errorf("argument %q must be at most %d bytes", "subject", MaxSubjectBytes)
	}
	if len(body) > MaxBodyBytes {
		return nil, fmt.Errorf("argument %q must be at most %d bytes", "body", MaxBodyBytes)
	}
	return json.Marshal(value)
}

type sendToolProvider struct {
	outbox outbox
}

func NewToolProvider(adapters ...*Adapter) toolbroker.ToolProvider {
	return newToolProvider(NewOutbox(adapters))
}

func newToolProvider(outbox outbox) *sendToolProvider {
	return &sendToolProvider{outbox: outbox}
}

func (p *sendToolProvider) Tools() []toolbroker.ToolRegistration {
	return []toolbroker.ToolRegistration{{
		Definition:    SendToolDefinition(),
		Adapter:       p.adapter,
		Rule:          SendToolRule(),
		ApprovalGuard: p.guard,
	}}
}

func (p *sendToolProvider) route(sessionID string) (Route, bool) {
	if p == nil || p.outbox == nil {
		return Route{}, false
	}
	route, ok := p.outbox.RouteOf(sessionID)
	return route, ok && route.Account != "" && route.Sender != ""
}

func (p *sendToolProvider) guard(_ context.Context, request *toolbroker.ToolRequest) (bool, error) {
	if request == nil {
		return true, toolbroker.Errorf(toolbroker.ResultInvalidArgument, "request must not be nil")
	}
	var args sendArguments
	if err := json.Unmarshal(request.Arguments, &args); err != nil {
		return true, err
	}
	route, ok := p.route(request.SessionID)
	return !ok || args.To != route.Sender, nil
}

func (p *sendToolProvider) adapter(ctx context.Context, request *toolbroker.ToolRequest, _ approval.Constraints) (toolbroker.ToolResult, error) {
	if request == nil {
		return toolbroker.ToolResult{}, toolbroker.Errorf(toolbroker.ResultInvalidArgument, "request must not be nil")
	}
	if request.PrincipalClass == principal.ClassPublic {
		return toolbroker.ToolResult{}, toolbroker.Errorf(toolbroker.ResultToolTierDenied, "email_send is not available to public principals")
	}
	var args sendArguments
	if err := json.Unmarshal(request.Arguments, &args); err != nil || !principal.IsPlainEmailAddress(args.To) {
		return toolbroker.ToolResult{}, toolbroker.Errorf(toolbroker.ResultInvalidArgument, "to must be one plain email address")
	}
	route, ok := p.route(request.SessionID)
	if !ok {
		return toolbroker.ToolResult{}, toolbroker.Errorf(toolbroker.ResultCapabilityUnavailable, "this session has no email account to send from")
	}
	messageID, err := p.outbox.Send(ctx, &Outgoing{
		SessionID:      request.SessionID,
		IdempotencyKey: request.IdempotencyKey,
		To:             args.To,
		Subject:        args.Subject,
		Body:           args.Body,
	})
	if err != nil {
		return toolbroker.ToolResult{}, mapSendError(ctx, err)
	}
	encoded, err := json.Marshal(map[string]string{"status": "sent", "account": route.Account, "message_id": messageID})
	if err != nil {
		return toolbroker.ToolResult{}, err
	}
	return toolbroker.ToolResult{Class: toolbroker.ResultOK, Output: encoded}, nil
}

func mapSendError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if _, coded := fault.CodeOf[toolbroker.ResultClass](err); coded {
		return err
	}
	if errors.Is(err, ErrNoRoute) {
		return toolbroker.Errorf(toolbroker.ResultCapabilityUnavailable, "this session has no email account to send from")
	}
	switch code, _ := fault.CodeOf[ErrorCode](err); code {
	case ErrorCodeMessageInvalid, ErrorCodeInvalidArgument:
		return toolbroker.Errorf(toolbroker.ResultInvalidArgument, "the message cannot be sent as written")
	case ErrorCodeDeliveryAmbiguous:
		return toolbroker.Errorf(toolbroker.ResultExecutionFailed, "the delivery outcome is unknown and the message was not resent")
	default:
		return toolbroker.Errorf(toolbroker.ResultExecutionFailed, "email could not be sent")
	}
}
