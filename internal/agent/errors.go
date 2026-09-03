package agent

import (
	"errors"
	"fmt"
)

type ErrorCode string

const (
	ErrorCodeDefinitionInvalid ErrorCode = "agent_definition_invalid"
	ErrorCodeDuplicateID       ErrorCode = "agent_duplicate_id"
	ErrorCodeUnknownCapability ErrorCode = "agent_unknown_capability"
	ErrorCodeUnknownTool       ErrorCode = "agent_unknown_tool"
	ErrorCodeUnknownModelRoute ErrorCode = "agent_unknown_model_route"
	ErrorCodeNotFound          ErrorCode = "agent_not_found"
	ErrorCodeResolutionFailed  ErrorCode = "agent_resolution_failed"
)

type Error struct {
	Code   ErrorCode
	Detail string
}

func (e *Error) Error() string {
	if e.Detail == "" {
		return string(e.Code)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Detail)
}

func codedError(code ErrorCode, detail string) *Error {
	return &Error{Code: code, Detail: detail}
}

func CodeOf(err error) (ErrorCode, bool) {
	var coded *Error
	if errors.As(err, &coded) {
		return coded.Code, true
	}
	return "", false
}
