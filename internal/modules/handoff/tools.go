package handoff

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/toolbroker"
	"github.com/anggasct/aura/internal/platform/tools"
)

const (
	ToolName          = "handoff_to_owner"
	handoffResultNote = "The owner was notified and will reply in this conversation. Do not continue the request yourself; tell the person the owner will follow up."
)

type Request struct {
	SessionID   string
	PrincipalID string
	AgentID     string
	Reason      string
}

type Opener interface {
	OpenHandoff(ctx context.Context, request *Request) error
}

func ToolDefinition() tools.Definition {
	return tools.Definition{
		Name:          ToolName,
		Version:       tools.Version,
		Schema:        []byte(`{"$schema":"https://json-schema.org/draft/2020-12/schema","$id":"aura://tools/handoff_to_owner/v1","type":"object","properties":{"reason":{"type":"string","minLength":1,"maxLength":500}},"required":["reason"],"additionalProperties":false}`),
		OptIn:         true,
		Orchestration: true,
		Validator:     validateHandoffArguments,
	}
}

func ToolRule() approval.Rule {
	return approval.Rule{
		ToolName:    ToolName,
		ToolVersion: tools.Version,
		AllowedTrust: []approval.TrustLabel{
			approval.TrustOwnerInput,
			approval.TrustTrustedConfiguration,
			approval.TrustDerivedUntrusted,
		},
		Constraints: approval.Constraints{
			MaxOutputBytes: 64 * 1024,
			Timeout:        30 * time.Second,
		},
	}
}

func ToolRegistration(opener Opener) toolbroker.ToolRegistration {
	return toolbroker.ToolRegistration{
		Definition: ToolDefinition(),
		Adapter:    NewToolAdapter(opener),
		Rule:       ToolRule(),
	}
}

type toolProvider struct {
	opener Opener
}

func NewToolProvider(opener Opener) toolbroker.ToolProvider {
	return &toolProvider{opener: opener}
}

func (p *toolProvider) Tools() []toolbroker.ToolRegistration {
	return []toolbroker.ToolRegistration{ToolRegistration(p.opener)}
}

func NewToolAdapter(opener Opener) toolbroker.Adapter {
	return func(ctx context.Context, request *toolbroker.ToolRequest, _ approval.Constraints) (toolbroker.ToolResult, error) {
		if request == nil {
			return toolbroker.ToolResult{}, toolbroker.Errorf(toolbroker.ResultInvalidArgument, "request must not be nil")
		}
		if request.PrincipalClass == "" || request.PrincipalClass == principal.ClassOwner {
			return toolbroker.ToolResult{}, toolbroker.Errorf(toolbroker.ResultPolicyDenied, "handoff is only for trusted and public principals")
		}
		if opener == nil {
			return toolbroker.ToolResult{}, toolbroker.Errorf(toolbroker.ResultCapabilityUnavailable, "handoff is not available")
		}
		var args struct {
			Reason string `json:"reason"`
		}
		if err := json.Unmarshal(request.Arguments, &args); err != nil || strings.TrimSpace(args.Reason) == "" {
			return toolbroker.ToolResult{}, toolbroker.Errorf(toolbroker.ResultInvalidArgument, "reason must be a non-empty string")
		}
		if err := opener.OpenHandoff(ctx, &Request{
			SessionID:   request.SessionID,
			PrincipalID: request.PrincipalID,
			AgentID:     request.AgentID,
			Reason:      strings.TrimSpace(args.Reason),
		}); err != nil {
			return toolbroker.ToolResult{}, toolbroker.Errorf(toolbroker.ResultCapabilityUnavailable, "owner could not be notified")
		}
		encoded, err := json.Marshal(map[string]string{"status": "handed_off", "note": handoffResultNote})
		if err != nil {
			return toolbroker.ToolResult{}, err
		}
		return toolbroker.ToolResult{Class: toolbroker.ResultOK, Output: encoded}, nil
	}
}

func validateHandoffArguments(raw json.RawMessage) (json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("arguments must be valid JSON: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("arguments must contain one JSON value")
		}
		return nil, fmt.Errorf("arguments have trailing data: %w", err)
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, errors.New("arguments must be a JSON object")
	}
	for key := range object {
		if key != "reason" {
			return nil, fmt.Errorf("unknown argument %q", key)
		}
	}
	rawReason, ok := object["reason"]
	if !ok {
		return nil, fmt.Errorf("argument %q is required", "reason")
	}
	reason, ok := rawReason.(string)
	if !ok || reason == "" {
		return nil, fmt.Errorf("argument %q must be a non-empty string", "reason")
	}
	return json.Marshal(object)
}
