package toolbroker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/anggasct/aura/internal/platform/approval"
)

const (
	outcomeResultKey  = "result"
	outcomeFailureKey = "failure"
)

type recordedFailure struct {
	Class  ResultClass `json:"class"`
	Detail string      `json:"detail"`
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

func encodeOutcomeResult(result json.RawMessage) ([]byte, error) {
	return json.Marshal(map[string]json.RawMessage{outcomeResultKey: result})
}

func encodeOutcomeFailure(class ResultClass, detail string) ([]byte, error) {
	return json.Marshal(map[string]recordedFailure{outcomeFailureKey: {Class: class, Detail: detail}})
}

func unreadableOutcome() error {
	return errorf(ResultExecutionFailed, "recorded tool outcome is not readable")
}

func decodeOutcome(raw []byte) (json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || len(fields) != 1 {
		return nil, unreadableOutcome()
	}
	if result, ok := fields[outcomeResultKey]; ok {
		return result, nil
	}
	encoded, ok := fields[outcomeFailureKey]
	if !ok {
		return nil, unreadableOutcome()
	}
	var failure recordedFailure
	if err := json.Unmarshal(encoded, &failure); err != nil || !failure.Class.stable() || failure.Class == ResultOK {
		return nil, unreadableOutcome()
	}
	return nil, &Error{Class: failure.Class, Detail: failure.Detail}
}

func (b *Broker) readOutcome(raw []byte) (json.RawMessage, error) {
	result, err := decodeOutcome(raw)
	if err == nil {
		return result, nil
	}
	var failure *Error
	if !errors.As(err, &failure) {
		return nil, err
	}
	return nil, &Error{Class: failure.Class, Detail: string(redact([]byte(failure.Detail), b.secretValues()))}
}

func (b *Broker) recordApproval(action func(ctx context.Context) ([]byte, error)) func(ctx context.Context) ([]byte, error) {
	return func(ctx context.Context) ([]byte, error) {
		result, err := action(ctx)
		if err == nil {
			return encodeOutcomeResult(result)
		}
		if ctx.Err() != nil {
			return nil, err
		}
		var coded *Error
		if !errors.As(mapApprovalError(err), &coded) || !coded.Class.stable() {
			return nil, err
		}
		return encodeOutcomeFailure(coded.Class, string(redact([]byte(coded.Detail), b.secretValues())))
	}
}

type journaledTool struct {
	canonical   *ToolRequest
	constraints approval.Constraints
	adapter     Adapter
	effectful   bool
}

func (b *Broker) runJournaledTool(ctx context.Context, call *journaledTool) ([]byte, error) {
	var result ToolResult
	var err error
	if call.effectful {
		result, err = b.executeEffect(ctx, call.canonical, call.constraints, call.adapter)
	} else {
		result, err = call.adapter(ctx, call.canonical, call.constraints)
	}
	if ctx.Err() != nil && (err != nil || (result.Class != "" && result.Class != ResultOK)) {
		return nil, b.interruption(toolKey(call.canonical), result.Class, err)
	}
	if err != nil {
		classified := b.classifyExecutionError(ctx, call.canonical, err)
		return encodeOutcomeFailure(classified.Class, classified.Detail)
	}
	if len(result.Output) > 0 {
		result.Output = redact(result.Output, b.secretValues())
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return encodeOutcomeFailure(ResultExecutionFailed, fmt.Sprintf("tool %q returned output that is not valid JSON", toolKey(call.canonical)))
	}
	return encodeOutcomeResult(encoded)
}

func (b *Broker) interruption(key string, class ResultClass, err error) error {
	cause := err
	if cause == nil {
		cause = fmt.Errorf("tool %q was interrupted before it finished: its result class is %s", key, class)
	}
	return &redactedError{text: string(redact([]byte(cause.Error()), b.secretValues())), cause: cause}
}
