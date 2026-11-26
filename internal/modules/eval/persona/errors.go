package personaeval

import (
	"fmt"

	"github.com/anggasct/aura/internal/platform/fault"
)

type ErrorCode string

const (
	ErrorCodeInvalid         ErrorCode = "persona_eval_invalid"
	ErrorCodeInvalidArgument ErrorCode = "invalid_argument"
	ErrorCodeFailed          ErrorCode = "persona_eval_failed"
)

func invalid(format string, args ...any) error {
	return fault.New(ErrorCodeInvalid, fmt.Sprintf(format, args...))
}
