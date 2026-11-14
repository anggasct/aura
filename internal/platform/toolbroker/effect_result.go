package toolbroker

import (
	"bytes"
	"context"
	"encoding/json"

	"github.com/anggasct/aura/internal/platform/effect"
	"github.com/anggasct/aura/internal/platform/fault"
)

func encodeRecord(result *ToolResult) ([]byte, error) {
	if result == nil {
		return nil, errorf(ResultInvalidArgument, "tool result must not be nil")
	}
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(result); err != nil {
		return nil, &Error{Class: ResultExecutionFailed, Detail: "encode recorded result", Cause: err}
	}
	return bytes.TrimSuffix(buffer.Bytes(), []byte("\n")), nil
}

func decodeRecord(record []byte) (ToolResult, error) {
	var result ToolResult
	if err := json.Unmarshal(record, &result); err != nil {
		return ToolResult{}, &Error{Class: ResultExecutionFailed, Detail: "decode recorded result", Cause: err}
	}
	return result, nil
}

func recordOutputLimit(result *ToolResult) (int64, error) {
	if result == nil {
		return 0, errorf(ResultInvalidArgument, "tool result must not be nil")
	}
	probe := *result
	probe.Output = json.RawMessage("0")
	probe.Truncated = false
	encoded, err := encodeRecord(&probe)
	if err != nil {
		return 0, err
	}
	return effect.MaxResultBytes - int64(len(encoded)-1), nil
}

func (b *Broker) replayRecorded(ctx context.Context, request *ToolRequest, intent *effect.Intent) (ToolResult, error) {
	if request == nil {
		return ToolResult{}, errorf(ResultInvalidArgument, "tool request must not be nil")
	}
	if intent == nil {
		return ToolResult{}, errorf(ResultInvalidArgument, "effect intent must not be nil")
	}
	if intent.SessionID != request.SessionID {
		return ToolResult{}, errorf(ResultPolicyDenied, "recorded result of tool %q belongs to another session", request.ToolName)
	}
	record, err := b.effects.Journal().RecordedResult(ctx, intent)
	if err != nil {
		code, coded := fault.CodeOf[effect.ErrorCode](err)
		switch {
		case coded && code == effect.ErrorCodeResultUnavailable:
			return ToolResult{}, errorf(ResultExecutionFailed, "effectful tool result is unavailable for a replayed request")
		case coded && code == effect.ErrorCodeResultCorrupt:
			return ToolResult{}, corruptRecord(request)
		}
		return ToolResult{}, err
	}
	result, err := decodeRecord(record)
	if err != nil || result.ToolName != request.ToolName || result.ToolVersion != request.ToolVersion || !result.Class.stable() {
		return ToolResult{}, corruptRecord(request)
	}
	return result, nil
}

func corruptRecord(request *ToolRequest) error {
	return errorf(ResultExecutionFailed, "recorded result of tool %q does not match its digest", request.ToolName)
}
