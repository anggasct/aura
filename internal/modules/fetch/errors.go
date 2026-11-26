package fetch

import (
	"fmt"

	"github.com/anggasct/aura/internal/platform/fault"
)

type ErrorCode string

const (
	ErrorCodeHTMLParseFailed ErrorCode = "html_parse_failed"
	ErrorCodeInvalidArgument ErrorCode = "invalid_argument"
)

func errorf(code ErrorCode, format string, args ...any) error {
	return fault.New(code, fmt.Sprintf(format, args...))
}
