package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/anggasct/aura/internal/approval"
	"github.com/anggasct/aura/internal/toolbroker"
)

func FormatToolName(serverName, toolName string) string {
	return fmt.Sprintf("mcp_%s_%s", serverName, toolName)
}

func MakeValidator(schema json.RawMessage, maxMsgSize int64) func(json.RawMessage) (json.RawMessage, error) {
	var resolved *jsonschema.Resolved
	var schemaErr error
	if len(bytes.TrimSpace(schema)) > 0 && string(bytes.TrimSpace(schema)) != "null" {
		var parsed jsonschema.Schema
		if err := json.Unmarshal(schema, &parsed); err != nil {
			schemaErr = err
		} else if r, err := parsed.Resolve(nil); err != nil {
			schemaErr = err
		} else {
			resolved = r
		}
	}
	return func(raw json.RawMessage) (json.RawMessage, error) {
		if len(raw) == 0 {
			raw = json.RawMessage("{}")
		}
		if !json.Valid(raw) {
			return nil, errors.New("invalid JSON arguments")
		}
		if maxMsgSize > 0 && int64(len(raw)) > maxMsgSize {
			return nil, fmt.Errorf("arguments size %d exceeds max message size %d", len(raw), maxMsgSize)
		}
		if schemaErr != nil {
			return nil, fmt.Errorf("invalid tool schema: %w", schemaErr)
		}
		if resolved != nil {
			var instance any
			if err := json.Unmarshal(raw, &instance); err != nil {
				return nil, errors.New("invalid JSON arguments")
			}
			if err := resolved.Validate(instance); err != nil {
				return nil, fmt.Errorf("arguments violate tool schema: %w", err)
			}
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

		if maxBytes > 0 && int64(len(outputBytes)) > maxBytes {
			return toolbroker.ToolResult{}, toolbroker.Errorf(toolbroker.ResultExecutionFailed, "tool result size %d exceeds limit %d", len(outputBytes), maxBytes)
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
		}, nil
	}
}
