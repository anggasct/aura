package runtimeengine

import (
	"fmt"

	"github.com/anggasct/aura/internal/runtime"
)

func codedError(code runtime.ErrorCode, detail string, cause error) error {
	if cause == nil {
		return &runtime.Error{Code: code, Detail: detail}
	}
	return fmt.Errorf("%w: %w", &runtime.Error{Code: code, Detail: detail}, cause)
}

func invalidArgument(detail string) error {
	return &runtime.Error{Code: runtime.ErrorCodeInvalidArgument, Detail: detail}
}
