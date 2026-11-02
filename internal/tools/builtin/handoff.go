package toolsbuiltin

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"

	"github.com/anggasct/aura/internal/approval"
	"github.com/anggasct/aura/internal/principal"
	"github.com/anggasct/aura/internal/toolbroker"
)

const handoffResultNote = "The owner was notified and will reply in this conversation. Do not continue the request yourself; tell the person the owner will follow up."

type HandoffRequest struct {
	SessionID   string
	PrincipalID string
	AgentID     string
	Reason      string
}

type HandoffOpener interface {
	OpenHandoff(ctx context.Context, request *HandoffRequest) error
}

type handoffHolder struct {
	opener atomic.Pointer[HandoffOpener]
}

func (h *handoffHolder) adapter(ctx context.Context, request *toolbroker.ToolRequest, _ approval.Constraints) (toolbroker.ToolResult, error) {
	if request == nil {
		return toolbroker.ToolResult{}, toolbroker.Errorf(toolbroker.ResultInvalidArgument, "request must not be nil")
	}
	if request.PrincipalClass == "" || request.PrincipalClass == principal.ClassOwner {
		return toolbroker.ToolResult{}, toolbroker.Errorf(toolbroker.ResultPolicyDenied, "handoff is only for trusted and public principals")
	}
	opener := h.opener.Load()
	if opener == nil {
		return toolbroker.ToolResult{}, toolbroker.Errorf(toolbroker.ResultCapabilityUnavailable, "handoff is not available")
	}
	var args struct {
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(request.Arguments, &args); err != nil || strings.TrimSpace(args.Reason) == "" {
		return toolbroker.ToolResult{}, toolbroker.Errorf(toolbroker.ResultInvalidArgument, "reason must be a non-empty string")
	}
	if err := (*opener).OpenHandoff(ctx, &HandoffRequest{
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

func (e *Executor) SetHandoffOpener(opener HandoffOpener) {
	if e == nil || e.handoff == nil || opener == nil {
		return
	}
	e.handoff.opener.Store(&opener)
}
