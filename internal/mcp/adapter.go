package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/anggasct/aura/internal/approval"
	"github.com/anggasct/aura/internal/toolbroker"
)

func FormatToolName(serverName, toolName string) string {
	return fmt.Sprintf("mcp_%s_%s", serverName, toolName)
}

func MakeValidator(schema json.RawMessage, maxMsgSize int64) func(json.RawMessage) (json.RawMessage, error) {
	return func(raw json.RawMessage) (json.RawMessage, error) {
		if len(raw) == 0 {
			return json.RawMessage("{}"), nil
		}
		if !json.Valid(raw) {
			return nil, errors.New("invalid JSON arguments")
		}
		if maxMsgSize > 0 && int64(len(raw)) > maxMsgSize {
			return nil, fmt.Errorf("arguments size %d exceeds max message size %d", len(raw), maxMsgSize)
		}
		return raw, nil
	}
}

func NewAdapter(client *Client, toolName string, maxMessageSize int64) toolbroker.Adapter {
	return func(ctx context.Context, request *toolbroker.ToolRequest, constraints approval.Constraints) (toolbroker.ToolResult, error) {
		if client == nil {
			return toolbroker.ToolResult{}, toolbroker.Errorf(toolbroker.ResultCapabilityUnavailable, "client is not available")
		}
		if request == nil {
			return toolbroker.ToolResult{}, toolbroker.Errorf(toolbroker.ResultInvalidArgument, "request must not be nil")
		}

		callCtx := ctx
		if constraints.Timeout > 0 {
			var cancel context.CancelFunc
			callCtx, cancel = context.WithTimeout(ctx, constraints.Timeout)
			defer cancel()
		}

		res, err := client.CallTool(callCtx, toolName, request.Arguments)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				return toolbroker.ToolResult{}, toolbroker.Errorf(toolbroker.ResultDeadlineExceeded, "tool call timed out")
			}
			return toolbroker.ToolResult{}, toolbroker.Errorf(toolbroker.ResultExecutionFailed, "tool call failed: %v", err)
		}

		maxBytes := constraints.MaxOutputBytes
		if maxBytes <= 0 || (maxMessageSize > 0 && maxMessageSize < maxBytes) {
			maxBytes = maxMessageSize
		}

		outputBytes, err := json.Marshal(res)
		if err != nil {
			return toolbroker.ToolResult{}, toolbroker.Errorf(toolbroker.ResultExecutionFailed, "failed to marshal tool result: %v", err)
		}

		truncated := false
		if maxBytes > 0 && int64(len(outputBytes)) > maxBytes {
			outputBytes = outputBytes[:maxBytes]
			truncated = true
		}

		resultClass := toolbroker.ResultOK
		if res.IsError {
			resultClass = toolbroker.ResultExecutionFailed
		}

		return toolbroker.ToolResult{
			ToolName:    request.ToolName,
			ToolVersion: request.ToolVersion,
			Class:       resultClass,
			Untrusted:   true,
			Output:      outputBytes,
			Truncated:   truncated,
		}, nil
	}
}
